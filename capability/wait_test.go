package capability

import (
	"encoding/json"
	"testing"
)

func TestApprovalWaitIDBindsInvocationIdentity(t *testing.T) {
	t.Parallel()

	base := Request{
		CallID:              "call-1",
		Tool:                "run_command",
		Capabilities:        []Name{ProcessExecute},
		ArgumentsDigest:     "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ExtensionID:         "qed.process",
		ExtensionGeneration: 2,
	}
	want := approvalWaitID(base)
	mutations := []Request{
		func() Request { value := base; value.CallID = "call-2"; return value }(),
		func() Request { value := base; value.Tool = "other"; return value }(),
		func() Request { value := base; value.Capabilities = []Name{FilesystemWrite}; return value }(),
		func() Request {
			value := base
			value.ArgumentsDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			return value
		}(),
		func() Request { value := base; value.ExtensionID = "other"; return value }(),
		func() Request { value := base; value.ExtensionGeneration = 3; return value }(),
		func() Request {
			value := base
			value.Preview = &ApprovalPreview{Summary: "changed preview"}
			return value
		}(),
	}
	for _, mutation := range mutations {
		if got := approvalWaitID(mutation); got == want {
			t.Fatalf("approvalWaitID did not bind mutation: %#v", mutation)
		}
	}
	reordered := base
	reordered.Capabilities = []Name{ProcessExecute, FilesystemRead}
	baseWithRead := base
	baseWithRead.Capabilities = []Name{FilesystemRead, ProcessExecute}
	if approvalWaitID(reordered) != approvalWaitID(baseWithRead) {
		t.Fatal("approvalWaitID depends on capability input order")
	}
}

func TestApprovalArgumentsDigestUsesExactArguments(t *testing.T) {
	t.Parallel()

	request := Request{Arguments: json.RawMessage(`{"argv":["go","test"]}`)}
	digest, err := approvalArgumentsDigest(request)
	if err != nil || digest != "sha256:8f35c48d72752a58a1d5b9f7a7788311008eaef383c6fd1d5b34fd5b897bcd2c" {
		t.Fatalf("approvalArgumentsDigest() = %q, %v", digest, err)
	}
	request.ArgumentsDigest = "sha256:not-a-digest"
	if _, err := approvalArgumentsDigest(request); err == nil {
		t.Fatal("approvalArgumentsDigest accepted invalid digest")
	}
}
