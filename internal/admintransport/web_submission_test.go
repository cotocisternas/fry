//go:build linux

package admintransport

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestSubmissionUIRequestAndStaleResponse(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node required for frontend behavior test")
	}
	raw, err := os.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(raw)
	start := strings.Index(js, "function senderEntries(")
	end := strings.Index(js, "form('submission-create-form'")
	if start < 0 || end < start {
		t.Fatal("submission behavior seam unavailable")
	}
	requireSubmissionUIBehavior(t, node, js, start, end)
	html, err := os.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(html), `id="submission-create-form"`) ||
		!strings.Contains(string(html), `name="enabled" type="checkbox" checked`) {
		t.Fatal("creation form/default enabled absent")
	}
	if !strings.Contains(js, "'submission-create-form', 'submission-policy-form', 'submission-password-form'") {
		t.Fatal("organization switch does not reset submission forms")
	}
}

func requireSubmissionUIBehavior(t *testing.T, node, js string, start, end int) {
	t.Helper()
	script := `const assert=require('node:assert/strict');` +
		"\n" +
		`let organization='org-a',viewGeneration=1,identity={},staleView=Symbol('stale'),` +
		`calls=[],shows=0,notices=0;` +
		"\n" +
		`const show=async()=>{shows++},notice=()=>{notices++};` +
		"\n" +
		`let pending,fail=false;` +
		"\n" +
		`const api=async(path,data)=>{assert.equal(form.elements.password.value,'');` +
		`calls.push({path,data});await new Promise(resolve=>pending=resolve);if(fail)throw ` +
		`Error('failure')};` +
		"\n" +
		`const form={elements:{username:{value:'robot'},password:{value:'synthetic-secret'},` +
		`authorized_senders:{value:' One@example.test,\nTwo@example.test '},` +
		`authorized_domains:{value:' example.test\n'},enabled:{checked:true}},resets:0,` +
		`reset(){this.resets++}};` +
		"\n" + js[start:end] + "\n" +
		`(async()=>{` +
		"\n" +
		`let result=submissionEdit(form,true);assert.equal(calls[0].path,` +
		`'submission-accounts');assert.deepEqual(calls[0].data.authorized_senders,` +
		`['One@example.test','Two@example.test']);assert.equal(calls[0].data.enabled,true);` +
		`assert.equal(calls[0].data.password,'synthetic-secret');organization='org-b';` +
		`viewGeneration++;pending();await result;assert.equal(shows,0);` +
		`assert.equal(form.resets,0);assert.equal(notices,0);` +
		"\n" +
		`form.elements.password.value='';result=submissionEdit(form,false);` +
		`assert.equal(calls[1].path,'submission-policy');assert.equal('password' in ` +
		`calls[1].data,false);assert.equal('enabled' in calls[1].data,false);pending();await ` +
		`result;assert.equal(shows,1);` +
		"\n" +
		`form.elements.password.value='another-secret';fail=true;result=submissionEdit(form,` +
		`true);assert.equal(form.elements.password.value,'');pending();await ` +
		`assert.rejects(result,/failure/);assert.equal(form.elements.password.value,'');` +
		"\n" +
		`form.elements.password.value='third-secret';result=submissionEdit(form,true);` +
		`identity=null;viewGeneration++;pending();try{await result;assert.fail('stale error ` +
		`escaped')}catch(e){assert.equal(e,staleView)}` +
		"\n" +
		`})().catch(e=>{console.error(e);process.exitCode=1});`
	// Owner: admin maintainers; review by 2027-01-18.
	// #nosec G204 -- node is resolved by LookPath; the script is repository-owned test code, with no external input.
	output, err := exec.CommandContext(t.Context(), node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("submission UI behavior failed: %s", output)
	}
}
