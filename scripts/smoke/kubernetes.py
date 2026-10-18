#!/usr/bin/env python3
"""Disposable kind integration rehearsal. Never targets an existing kubecontext.

Uses the container smoke's synthetic PKI/configuration and exact-byte protocol
checks. Only this runner's uniquely named cluster and private files are removed.
"""
import base64
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import tarfile
import time

from scripts.smoke import container_processes as fixture

# Restore a runnable pin only after kind publishes a verified >=1.37.1 digest.
# See docs/validation/image-remediation-2026-10-18.md for upstream evidence.
NODE = None
NODE_SECURITY_HOLD = ('Security hold: Kubernetes >=1.37.1 is required, but no official '
                      'kindest/node image with a verified digest is published. '
                      'Update NODE to a verified patched digest and provision it locally; '
                      'this rehearsal never pulls images.')
LABEL = 'app.kubernetes.io/part-of'


def progress(message):
    print('[kind rehearsal] ' + message, flush=True)


def execute(*args, timeout=30, check=True, data=None):
    """Bound every command and emit progress even for image loading/creation."""
    environment = os.environ.copy()
    environment['KIND_EXPERIMENTAL_PROVIDER'] = 'docker'
    with subprocess.Popen(args, env=environment, stdin=subprocess.PIPE if data is not None else subprocess.DEVNULL,
                          stdout=subprocess.PIPE, stderr=subprocess.PIPE) as process:
        deadline = time.monotonic() + timeout
        pending = data
        try:
            while True:
                try:
                    stdout, stderr = process.communicate(input=pending, timeout=min(20, max(.01, deadline - time.monotonic())))
                    break
                except subprocess.TimeoutExpired:
                    pending = None
                    if time.monotonic() >= deadline:
                        raise TimeoutError(args[0] + ' exceeded its bounded deadline')
                    progress('waiting for ' + ' '.join(args[:3]))
        except BaseException:
            process.kill()
            process.communicate()
            raise
    result = subprocess.CompletedProcess(args, process.returncode, stdout, stderr)
    if check and result.returncode:
        raise RuntimeError(args[0] + ' failed: ' + stderr.decode(errors='replace')[-4096:])
    return result


class Rehearsal(fixture.Rehearsal):
    def __init__(self, image, root):
        super().__init__(image, root)
        self.token = 'fry-kind-' + secrets.token_hex(6)
        self.cluster = self.token
        self.namespace = self.token
        self.kubeconfig = (root / 'kubeconfig').resolve()
        assert not self.kubeconfig.exists()
        self.context_name = 'kind-' + self.cluster
        self.db = 'database'
        self.coordinator = 'coordinator'
        self.members = ['member-a', 'member-b']
        self.postgres_image = self.token + ':postgres'
        self.postgres_tag_created = False
        self.cluster_attempted = False
        self.api_verified = False
        self.forwards = []
        self.ports = {}
        self.member_specs = []
        self.coordinator_spec = None
        self.report = {'profile': 'one-host-kind-synthetic', 'cluster': self.cluster,
                       'node_image': NODE, 'application_image': image,
                       'messages': 0, 'passed': False}

    def kube(self, *args, timeout=30, check=True, data=None, namespaced=True):
        assert self.kubeconfig.is_absolute() and self.kubeconfig.parent == self.root.resolve()
        assert self.kubeconfig.exists(), 'private kubeconfig missing; refusing ambient context'
        return execute('kubectl', '--kubeconfig', str(self.kubeconfig),
                       '--context', self.context_name, '--request-timeout=10s',
                       *(['--namespace', self.namespace] if namespaced else []),
                       *args, timeout=timeout, check=check, data=data)

    def get(self, resource, name=None, namespaced=True):
        args = ['get', resource]
        if name:
            args.append(name)
        return json.loads(self.kube(*args, '-o', 'json', namespaced=namespaced).stdout)

    def apply(self, value):
        assert self.api_verified, 'cluster identity not verified'
        self.kube('apply', '-f', '-', data=json.dumps(value).encode())

    def write_config(self, name, value):
        if name in ('member-a.json', 'member-b.json'):
            value['data_directory'] = '/var/lib/fry/data'
        super().write_config(name, value)

    def metadata(self, name):
        return {'name': name, 'namespace': self.namespace, 'labels': {LABEL: self.token, 'app': name}}

    def claim(self, name):
        return {'apiVersion': 'v1', 'kind': 'PersistentVolumeClaim', 'metadata': self.metadata(name),
                'spec': {'accessModes': ['ReadWriteOnce'], 'storageClassName': 'standard',
                         'resources': {'requests': {'storage': '1Gi'}}}}

    def service(self, name, ports):
        return {'apiVersion': 'v1', 'kind': 'Service', 'metadata': self.metadata(name),
                'spec': {'type': 'ClusterIP', 'selector': {'app': name, LABEL: self.token},
                         'ports': [{'name': label, 'port': port, 'targetPort': port, 'protocol': 'TCP'}
                                   for label, port in ports]}}

    def pod(self, name, binary, data_claim=None):
        items = ['ca.crt', name + '.json', name + '.crt', name + '.key']
        if name == self.coordinator or binary == 'initializer':
            items = ['ca.crt', 'coordinator.json', 'coordinator.crt', 'coordinator.key',
                     'coordinator-client.crt', 'coordinator-client.key']
        container = {
            'name': name, 'image': self.image, 'imagePullPolicy': 'Never',
            'command': ['/usr/local/bin/' + ('fry' if binary == 'initializer' else binary)],
            'args': ['-config', '/etc/fry/' + ('coordinator' if binary == 'initializer' else name) + '.json'],
            'env': [{'name': 'FRY_DATABASE_URL', 'valueFrom': {'secretKeyRef': {'name': 'fixture', 'key': 'database_url'}}},
                    {'name': 'FRY_CONTAINER_IMAP_PASSWORD', 'valueFrom': {'secretKeyRef': {'name': 'fixture', 'key': 'imap_password'}}}],
            'securityContext': {'readOnlyRootFilesystem': True, 'allowPrivilegeEscalation': False,
                                'capabilities': {'drop': ['ALL']}},
            'resources': {'requests': {'cpu': '100m', 'memory': '128Mi'},
                          'limits': {'cpu': '1', 'memory': '512Mi'}},
            'volumeMounts': [{'name': 'runtime', 'mountPath': '/etc/fry', 'readOnly': True}]}
        if binary == 'initializer':
            container['args'].append('-init-only')
        else:
            port = 8443 if data_claim else 2525
            probe = {'tcpSocket': {'port': port}, 'timeoutSeconds': 2, 'periodSeconds': 2}
            container['startupProbe'] = {**probe, 'failureThreshold': 45}
            container['readinessProbe'] = {**probe, 'failureThreshold': 3}
        volumes = [{'name': 'runtime', 'secret': {'secretName': 'fixture', 'defaultMode': 0o444,
                                                 'items': [{'key': key, 'path': key} for key in items]}}]
        if data_claim:
            volumes.append({'name': 'data', 'persistentVolumeClaim': {'claimName': data_claim}})
            container['volumeMounts'].append({'name': 'data', 'mountPath': '/var/lib/fry'})
        return {'apiVersion': 'v1', 'kind': 'Pod', 'metadata': self.metadata(name),
                'spec': {'restartPolicy': 'Never', 'terminationGracePeriodSeconds': 60,
                         'automountServiceAccountToken': False,
                         'securityContext': {'runAsNonRoot': True, 'runAsUser': 65532, 'runAsGroup': 65532,
                                             'fsGroup': 65532, 'fsGroupChangePolicy': 'OnRootMismatch',
                                             'seccompProfile': {'type': 'RuntimeDefault'}},
                         'containers': [container], 'volumes': volumes}}

    def postgres(self):
        return {'apiVersion': 'v1', 'kind': 'Pod', 'metadata': self.metadata(self.db),
                'spec': {'restartPolicy': 'Never', 'terminationGracePeriodSeconds': 60,
                         'automountServiceAccountToken': False,
                         'securityContext': {'runAsNonRoot': True, 'runAsUser': 999, 'runAsGroup': 999,
                                             'fsGroup': 999, 'fsGroupChangePolicy': 'OnRootMismatch'},
                         'containers': [{'name': 'postgres', 'image': self.postgres_image, 'imagePullPolicy': 'Never',
                                         'env': [{'name': 'POSTGRES_PASSWORD', 'value': 'fry-test-only'},
                                                 {'name': 'POSTGRES_DB', 'value': 'fry_test'}],
                                         'resources': {'requests': {'cpu': '250m', 'memory': '256Mi'},
                                                       'limits': {'cpu': '1', 'memory': '1Gi'}},
                                         'readinessProbe': {'exec': {'command': ['pg_isready', '-h', '127.0.0.1', '-U', 'postgres', '-d', 'fry_test']},
                                                            'periodSeconds': 2, 'timeoutSeconds': 2},
                                         'volumeMounts': [{'name': 'data', 'mountPath': '/var/lib/postgresql'}]}],
                         'volumes': [{'name': 'data', 'persistentVolumeClaim': {'claimName': 'database-data'}}]}}

    def wait_ready(self, name, seconds=90):
        deadline = time.monotonic() + seconds
        last_progress = 0
        while time.monotonic() < deadline:
            pod = self.get('pod', name)
            status = pod.get('status', {})
            assert status.get('phase') not in ('Failed', 'Succeeded'), name + ' exited: ' + self.logs(name)[-4096:]
            states = status.get('initContainerStatuses', []) + status.get('containerStatuses', [])
            for state in states:
                waiting = state.get('state', {}).get('waiting', {})
                if waiting.get('reason') in ('ErrImageNeverPull', 'ErrImagePull', 'ImagePullBackOff',
                                              'InvalidImageName', 'CreateContainerConfigError'):
                    raise AssertionError(name + ' cannot start: ' + waiting.get('reason', '') + ': '
                                         + waiting.get('message', '')[:2048])
            assert all(v.get('restartCount', 0) == 0 for v in states), name + ' unexpectedly restarted'
            if states and all(v.get('ready', False) for v in states):
                return
            if time.monotonic() - last_progress > 15:
                progress('waiting for pod ' + name)
                last_progress = time.monotonic()
            time.sleep(.5)
        raise AssertionError(name + ' readiness deadline: ' + self.logs(name)[-4096:])

    def logs(self, name):
        result = self.kube('logs', name, '--tail=150', check=False)
        return (result.stdout + result.stderr).decode(errors='replace')

    def sql(self, query):
        return self.kube('exec', self.db, '--', 'psql', '-U', 'postgres', '-d', 'fry_test',
                         '-v', 'ON_ERROR_STOP=1', '-Atc', query).stdout.decode().strip()

    def load_image(self, reference, filename, source):
        archive = self.root / filename
        progress('loading pinned/local image into uniquely named cluster: ' + reference)
        execute('docker', 'save', '--platform=linux/amd64', '--output', str(archive), reference, timeout=120)
        try:
            # Docker29 exposes index/manifest IDs via inspect; CRI exposes the
            # config digest. Derive that digest from the exact exported config.
            with tarfile.open(archive) as exported:
                records = json.load(exported.extractfile('manifest.json'))
                matches = [v for v in records if reference in v.get('RepoTags', [])]
                assert len(matches) == 1, 'archive lost or duplicated the exact image tag: ' + reference
                config_bytes = exported.extractfile(matches[0]['Config']).read(4 << 20)
                config = json.loads(config_bytes)
            assert config['architecture'] == source['Architecture'] == 'amd64'
            assert config['os'] == source['Os'] == 'linux'
            assert config['rootfs']['diff_ids'] == source['RootFS']['Layers'], 'exported image layers changed'
            defaults = {'User': '', 'WorkingDir': '', 'StopSignal': '', 'Env': [], 'Entrypoint': [],
                        'Cmd': [], 'Labels': {}, 'Volumes': {}, 'ExposedPorts': {}}
            for key, default in defaults.items():
                assert (config.get('config', {}).get(key) or default) == (source['Config'].get(key) or default), \
                    'exported image config changed: ' + key
            expected_id = 'sha256:' + hashlib.sha256(config_bytes).hexdigest()
            execute('kind', 'load', 'image-archive', str(archive), '--name', self.cluster, timeout=180)
            loaded = json.loads(execute('docker', 'exec', self.cluster + '-control-plane',
                                        'crictl', 'inspecti', reference).stdout)
            assert loaded['status']['id'] == expected_id, \
                'loaded image config identity differs from inspected source: ' + reference
            return loaded['status']['id']
        finally:
            archive.unlink(missing_ok=True)

    def setup(self):
        if NODE is None:
            raise RuntimeError(NODE_SECURITY_HOLD)
        assert execute('kind', 'version').stdout.decode().startswith('kind v0.33.0 '), 'kind0.33.0 required'
        version = json.loads(execute('kubectl', '--kubeconfig', str(self.kubeconfig),
                                     '--context', self.context_name, 'version', '--client', '-o', 'json').stdout)
        assert version['clientVersion']['minor'] == '37', 'kubectl1.37 required'
        endpoint = execute('docker', 'context', 'inspect', '--format', '{{.Endpoints.docker.Host}}').stdout.decode().strip()
        assert endpoint.startswith('unix://'), 'local Unix-socket Docker context required'
        assert not os.environ.get('DOCKER_HOST') or os.environ['DOCKER_HOST'].startswith('unix://'), \
            'remote DOCKER_HOST is outside this local rehearsal'
        arch = execute('docker', 'info', '--format', '{{.Architecture}}').stdout.decode().strip()
        assert arch in ('x86_64', 'amd64'), 'this first bounded runner requires Linux amd64'
        execute('docker', 'image', 'inspect', self.image)
        self.report['application_id'] = json.loads(execute('docker', 'image', 'inspect', self.image).stdout)[0]['Id']
        self.report['application_index_id'] = self.report['application_id']
        application_source = json.loads(execute('docker', 'image', 'inspect', '--platform=linux/amd64', self.image).stdout)[0]
        self.report['application_platform_manifest_id'] = application_source['Id']
        self.postgres_source = fixture.testdeps.image()
        source = json.loads(execute('docker', 'image', 'inspect', '--platform=linux/amd64', self.postgres_source).stdout)[0]
        assert source['Architecture'] == 'amd64' and source['Os'] == 'linux'
        assert re.fullmatch(r'sha256:[0-9a-f]{64}', source['Id']), 'PostgreSQL source must have a content identity'
        assert execute('docker', 'image', 'inspect', self.postgres_image, check=False).returncode, \
            'generated PostgreSQL alias already exists'
        self.postgres_tag_created = True
        execute('docker', 'tag', self.postgres_source, self.postgres_image)
        alias = json.loads(execute('docker', 'image', 'inspect', '--platform=linux/amd64', self.postgres_image).stdout)[0]
        assert alias['Id'] == source['Id'], 'temporary PostgreSQL alias changed source identity'
        self.report['postgres_source'] = self.postgres_source
        self.report['postgres_platform_manifest_id'] = source['Id']
        self.report['postgres_loaded_reference'] = self.postgres_image
        assert execute('docker', 'image', 'inspect', NODE, check=False).returncode == 0, \
            'Pinned kind node image must already exist locally; this rehearsal never pulls images'
        self.configure()
        kind_config = self.root / 'kind.json'
        kind_config.write_text(json.dumps({'kind': 'Cluster', 'apiVersion': 'kind.x-k8s.io/v1alpha4',
                                          'networking': {'apiServerAddress': '127.0.0.1', 'ipFamily': 'ipv4'},
                                          'nodes': [{'role': 'control-plane', 'image': NODE}]}))
        existing = execute('kind', 'get', 'clusters').stdout.decode().splitlines()
        assert self.cluster not in existing, 'generated cluster name collision'
        self.cluster_attempted = True
        progress('creating only ' + self.cluster)
        execute('kind', 'create', 'cluster', '--name', self.cluster, '--config', str(kind_config),
                '--kubeconfig', str(self.kubeconfig), '--wait', '120s', timeout=180)
        config = json.loads(self.kube('config', 'view', '-o', 'json', namespaced=False).stdout)
        assert len(config['contexts']) == 1 and config['current-context'] == self.context_name
        assert config['contexts'][0]['name'] == self.context_name and len(config['clusters']) == 1
        assert re.fullmatch(r'https://127\.0\.0\.1:[0-9]+', config['clusters'][0]['cluster']['server'])
        self.api_verified = True
        self.report['application_loaded_config_id'] = self.load_image(self.image, 'application.tar', application_source)
        self.report['postgres_loaded_config_id'] = self.load_image(self.postgres_image, 'postgres.tar', source)
        self.apply({'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': self.namespace, 'labels': {LABEL: self.token}}})
        secret = {path.name: base64.b64encode(path.read_bytes()).decode() for path in self.runtime.iterdir()}
        secret.update({'database_url': base64.b64encode(b'postgres://postgres:fry-test-only@database:5432/fry_test?sslmode=disable').decode(),
                       'imap_password': base64.b64encode(fixture.PASSWORD.encode()).decode()})
        self.apply({'apiVersion': 'v1', 'kind': 'Secret', 'metadata': self.metadata('fixture'), 'data': secret})
        for name in ['database-data', 'member-a-data', 'member-b-data']:
            self.apply(self.claim(name))
        for name, ports in [('database', [('postgres', 5432)]), ('member-a', [('member', 8443)]),
                            ('member-b', [('member', 8443)]), ('coordinator', [('smtp', 2525), ('imaps', 1993)])]:
            self.apply(self.service(name, ports))
        self.apply(self.postgres())
        self.wait_ready(self.db)
        progress('initializing fresh fixture schema once')
        init = self.pod('initialize', 'initializer')
        job = {'apiVersion': 'batch/v1', 'kind': 'Job', 'metadata': self.metadata('initialize'),
               'spec': {'backoffLimit': 0, 'activeDeadlineSeconds': 60,
                        'template': {'metadata': {'labels': {LABEL: self.token}}, 'spec': init['spec']}}}
        self.apply(job)
        self.kube('wait', '--for=condition=complete', 'job/initialize', '--timeout=65s', timeout=75)
        for name in self.members:
            value = self.pod(name, 'fry-member', name + '-data')
            self.member_specs.append(value)
            self.apply(value)
            self.wait_ready(name)
        assert self.sql('SELECT count(*) FROM fry_member_owner WHERE NOT closed') == '2'
        self.coordinator_spec = self.pod(self.coordinator, 'fry')
        self.apply(self.coordinator_spec)
        self.wait_ready(self.coordinator)
        self.start_forwards()
        self.verify_security()
        self.reject_unauthenticated_member()

    def reject_unauthenticated_member(self):
        progress('checking member TLS rejects a client without its certificate')
        private = 8443
        port = self.forward('member-a', private)
        try:
            with socket.create_connection(('127.0.0.1', port), timeout=5) as connection:
                try:
                    with self.context.wrap_socket(connection, server_hostname='127.0.0.1') as secured:
                        secured.sendall(b'GET /v1/identity HTTP/1.1\r\nHost: member-a\r\nConnection: close\r\n\r\n')
                        response = secured.recv(4096)
                        assert not response, 'member returned HTTP to unauthenticated TLS client: ' + repr(response)
                except fixture.ssl.SSLCertVerificationError:
                    raise
                except fixture.ssl.SSLError as error:
                    assert error.reason == 'TLSV13_ALERT_CERTIFICATE_REQUIRED', \
                        'unexpected TLS failure (not client-certificate rejection): ' + str(error)
        finally:
            process, output = self.forwards.pop()
            self.end_forward(process, output)

    def verify_security(self):
        observed = {}
        for name in [*self.members, self.coordinator]:
            pod = self.get('pod', name)
            security = pod['spec']['securityContext']
            assert security['runAsUser'] == 65532 and security['runAsGroup'] == 65532
            assert security['runAsNonRoot'] and pod['spec']['restartPolicy'] == 'Never'
            assert pod['spec']['terminationGracePeriodSeconds'] == 60
            for container in pod['spec']['containers']:
                context = container['securityContext']
                assert context['readOnlyRootFilesystem'] and not context['allowPrivilegeEscalation']
                assert context['capabilities']['drop'] == ['ALL']
            assert all(v['restartCount'] == 0 for v in pod['status']['containerStatuses'])
            observed[name] = {'uid': security['runAsUser'], 'read_only_root': True,
                              'restarts': [v['restartCount'] for v in pod['status']['containerStatuses']],
                              'image_ids': [v['imageID'] for v in pod['status']['containerStatuses']]}
        self.report['pods'] = observed
        self.report['pvc_bindings'] = {v['metadata']['name']: v['spec']['volumeName']
                                       for v in self.get('pvc')['items']}

    def forward(self, service, private):
        output = open(self.root / ('forward-' + service + '-' + str(private) + '.log'), 'w+b')
        args = ['kubectl', '--kubeconfig', str(self.kubeconfig), '--context', self.context_name,
                '--namespace', self.namespace, '--request-timeout=10s', 'port-forward',
                '--address', '127.0.0.1', 'service/' + service, ':' + str(private)]
        process = subprocess.Popen(args, stdout=output, stderr=output)
        self.forwards.append((process, output))
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            assert process.poll() is None, 'port-forward exited: ' + Path(output.name).read_text()
            match = re.search(r'Forwarding from 127\.0\.0\.1:([0-9]+) ->', Path(output.name).read_text())
            if match:
                return int(match.group(1))
            time.sleep(.1)
        raise AssertionError('loopback port-forward startup timeout')

    def start_forwards(self):
        self.stop_forwards()
        for private in (2525, 1993):
            self.ports[private] = self.forward('coordinator', private)

    def end_forward(self, process, output):
        if process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
        output.close()

    def stop_forwards(self):
        for process, output in self.forwards:
            self.end_forward(process, output)
        self.forwards.clear()
        self.ports.clear()

    def port(self, private):
        assert all(p.poll() is None for p, _ in self.forwards), 'port-forward exited'
        return self.ports[private]

    def stop_pod(self, name):
        self.kube('delete', 'pod', name, '--grace-period=60', '--wait=true', '--timeout=75s', timeout=85)

    def clean_restart(self):
        progress('gracefully deleting coordinator then both members; retaining PVCs')
        self.stop_forwards()
        self.stop_pod(self.coordinator)
        for name in self.members:
            self.stop_pod(name)
        assert self.sql('SELECT count(*) FROM fry_member_owner WHERE closed') == '2'
        assert self.sql('SELECT count(*) FROM fry_member_owner WHERE NOT closed') == '0'
        for value in self.member_specs:
            self.apply(value)
            self.wait_ready(value['metadata']['name'])
        assert self.sql('SELECT count(*) FROM fry_member_owner WHERE epoch=2 AND NOT closed') == '2'
        self.apply(self.coordinator_spec)
        self.wait_ready(self.coordinator)
        self.start_forwards()
        self.verify_security()
        self.verify()
        self.send(2)
        self.verify()

    def diagnostics(self):
        if not self.api_verified:
            return
        for resource in ['pods', 'pvc', 'events']:
            try:
                result = self.kube('get', resource, '-o', 'wide', check=False)
                print(result.stdout.decode(errors='replace')[-8192:], file=sys.stderr)
            except Exception as error:
                print('Diagnostics unavailable: ' + str(error), file=sys.stderr)
        for name in [self.db, *self.members, self.coordinator]:
            try:
                print(name + ': ' + self.logs(name)[-4096:], file=sys.stderr)
            except Exception:
                pass

    def cleanup(self):
        self.stop_forwards()
        errors = []
        if self.api_verified:
            # Normal ordered shutdown before deleting disposable local storage.
            for name in [self.coordinator, *self.members]:
                try:
                    self.kube('delete', 'pod', name, '--ignore-not-found', '--grace-period=60',
                              '--wait=true', '--timeout=75s', timeout=85)
                except Exception as error:
                    errors.append(name + ': ' + str(error))
            if self.report['passed']:
                try:
                    assert self.sql('SELECT count(*) FROM fry_member_owner WHERE NOT closed') == '0', \
                        'final member shutdown did not close every owner'
                except Exception as error:
                    errors.append('final owner closure: ' + str(error))
            try:
                self.kube('delete', 'pod', self.db, '--ignore-not-found', '--grace-period=60',
                          '--wait=true', '--timeout=75s', timeout=85)
            except Exception as error:
                errors.append(self.db + ': ' + str(error))
        try:
            if self.cluster_attempted:
                progress('deleting only generated cluster ' + self.cluster)
                execute('kind', 'delete', 'cluster', '--name', self.cluster,
                        '--kubeconfig', str(self.kubeconfig), timeout=120)
        finally:
            if self.postgres_tag_created:
                removed = execute('docker', 'image', 'rm', self.postgres_image, check=False)
                if removed.returncode and b'No such image:' not in removed.stderr:
                    raise RuntimeError('temporary image tag cleanup failed: ' + removed.stderr.decode(errors='replace')[-2048:])
                self.postgres_tag_created = False
        if errors:
            print('Cleanup diagnostics: ' + '; '.join(errors), file=sys.stderr)
            if self.report['passed']:
                raise RuntimeError('successful workload had incomplete graceful cleanup')


def main():
    if len(sys.argv) != 2:
        raise SystemExit('Usage: python3 -m scripts.smoke.kubernetes IMAGE_TAG')
    if NODE is None:
        raise SystemExit(NODE_SECURITY_HOLD)
    for name in ('docker', 'kind', 'kubectl', 'openssl'):
        if shutil.which(name) is None:
            raise SystemExit('Required command unavailable: ' + name)
    os.umask(0o077)
    signal.signal(signal.SIGINT, fixture.interrupted)
    signal.signal(signal.SIGTERM, fixture.interrupted)
    with tempfile.TemporaryDirectory(prefix='fry-kind-smoke-') as directory:
        rehearsal = Rehearsal(sys.argv[1], Path(directory).resolve())
        try:
            rehearsal.setup()
            progress('verifying TLS SMTP acceptance and exact IMAP originals')
            rehearsal.send(1)
            rehearsal.verify()
            rehearsal.clean_restart()
            rehearsal.report.update({'messages': len(rehearsal.expected), 'passed': True,
                                     'clean_same_pvc_reopen': True})
        except BaseException:
            rehearsal.diagnostics()
            raise
        finally:
            signal.signal(signal.SIGINT, signal.SIG_IGN)
            signal.signal(signal.SIGTERM, signal.SIG_IGN)
            rehearsal.cleanup()
        print(json.dumps(rehearsal.report, sort_keys=True), flush=True)


if __name__ == '__main__':
    main()
