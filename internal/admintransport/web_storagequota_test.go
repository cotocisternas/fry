//go:build linux

package admintransport

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestStorageQuotaUIDecimalAndStaleResponse(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node needed")
	}
	raw, err := os.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(raw)
	a := strings.Index(js, "form('storage-quota-form',")
	if a < 0 {
		t.Fatal("quota UI seam absent")
	}
	b := strings.Index(js[a:], "function senderEntries(")
	if b < 0 {
		t.Fatal("quota UI seam absent")
	}
	// The excerpt may register other forms too. Select the quota action by its
	// identity so inserting a neighboring form cannot replace the tested handler.
	script := `const assert=require('node:assert/strict');let action,organization='a',` +
		`viewGeneration=1,identity={},staleView=Symbol(),shows=0,notices=0,pending,data;` +
		"\n" +
		`const form=(id,fn)=>{if(id==='storage-quota-form')action=fn},show=async()=>shows++,` +
		`notice=()=>notices++,api=async(path,value)=>{assert.equal(path,'storage-quota');` +
		`data=value;await new Promise(r=>pending=r)};` +
		"\n" + js[a:a+b] + "\n" +
		`(async()=>{const f={elements:{disable_limit:{checked:false},` +
		`limit_bytes:{value:'9223372036854775807'}}};let result=action(f);` +
		`assert.equal(data.limit_bytes,'9223372036854775807');organization='b';` +
		`viewGeneration++;pending();await result;assert.equal(shows,0);assert.equal(notices,` +
		`0);f.elements.disable_limit.checked=true;result=action(f);` +
		`assert.equal(data.limit_bytes,null);pending();await result;assert.equal(shows,` +
		`1)})().catch(e=>{console.error(e);process.exitCode=1});`
	// Owner: admin maintainers; review by 2027-01-18.
	// #nosec G204 -- node is resolved by LookPath; the script is repository-owned test code, with no external input.
	out, err := exec.CommandContext(t.Context(), node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("quota UI: %s", out)
	}
	if strings.Contains(js, "Number(u.retained_bytes)") || strings.Contains(js, "parseInt(u.retained_bytes)") {
		t.Fatal("unsafe numeric conversion")
	}
}
