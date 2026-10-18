ARG BASE
FROM ${BASE} AS build
ARG DEBIAN_SNAPSHOT
ARG DEBIAN_SECURITY_SNAPSHOT
# Snapshot InRelease signatures authenticate the package index; apt verifies
# every package checksum against that index. The timestamp fixes all builddeps.
RUN rm -f /etc/apt/sources.list.d/* && \
    printf 'deb [check-valid-until=no signed-by=/usr/share/keyrings/debian-archive-keyring.gpg] http://snapshot.debian.org/archive/debian/%s trixie main\n' "$DEBIAN_SNAPSHOT" > /etc/apt/sources.list && \
    printf 'deb [check-valid-until=no signed-by=/usr/share/keyrings/debian-archive-keyring.gpg] http://snapshot.debian.org/archive/debian-security/%s trixie-security main\n' "$DEBIAN_SECURITY_SNAPSHOT" >> /etc/apt/sources.list && \
    apt-get update && apt-get dist-upgrade -y --no-install-recommends && \
    apt-get install -y --no-install-recommends \
      build-essential bison flex pkg-config libreadline-dev zlib1g-dev \
      libssl-dev libicu-dev liblz4-dev libzstd-dev bzip2 && \
    rm -rf /var/lib/apt/lists/*
COPY postgresql.tar.bz2 /tmp/postgresql.tar.bz2
ARG POSTGRES_SHA256
RUN printf '%s  /tmp/postgresql.tar.bz2\n' "$POSTGRES_SHA256" | sha256sum -c - && \
    mkdir /src && tar -xjf /tmp/postgresql.tar.bz2 --strip-components=1 -C /src && \
    cd /src && ./configure --prefix=/usr/local --with-openssl --with-lz4 --with-zstd && \
    make -j2 && make install

FROM ${BASE}
ARG DEBIAN_SNAPSHOT
ARG DEBIAN_SECURITY_SNAPSHOT
RUN rm -f /etc/apt/sources.list.d/* && \
    printf 'deb [check-valid-until=no signed-by=/usr/share/keyrings/debian-archive-keyring.gpg] http://snapshot.debian.org/archive/debian/%s trixie main\n' "$DEBIAN_SNAPSHOT" > /etc/apt/sources.list && \
    printf 'deb [check-valid-until=no signed-by=/usr/share/keyrings/debian-archive-keyring.gpg] http://snapshot.debian.org/archive/debian-security/%s trixie-security main\n' "$DEBIAN_SECURITY_SNAPSHOT" >> /etc/apt/sources.list && \
    apt-get update && apt-get dist-upgrade -y --no-install-recommends && \
    apt-get install -y --no-install-recommends \
      libreadline8t64 zlib1g libssl3t64 libicu76 liblz4-1 libzstd1 && \
    rm -rf /var/lib/apt/lists/* && \
    groupadd --gid 999 postgres && useradd --uid 999 --gid postgres --home-dir /var/lib/postgresql postgres && \
    mkdir -p /var/lib/postgresql/data && chown -R postgres:postgres /var/lib/postgresql
COPY --from=build /usr/local/ /usr/local/
COPY entrypoint.sh /usr/local/bin/fry-postgres
ENV PGDATA=/var/lib/postgresql/data
USER postgres
EXPOSE 5432
ENTRYPOINT ["/bin/sh", "/usr/local/bin/fry-postgres"]
CMD ["postgres"]
