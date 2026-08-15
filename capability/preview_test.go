package capability_test

import (
	"strings"
	"testing"

	"github.com/qed-runtime/qed/capability"
)

func TestApprovalPreviewValidationAndClone(t *testing.T) {
	t.Parallel()

	preview := &capability.ApprovalPreview{
		Summary: "Run workspace verification",
		Details: []capability.ApprovalPreviewDetail{
			{Label: "argv", Value: `["go","test","./..."]`},
			{Label: "cwd", Value: "."},
		},
	}
	if err := capability.ValidateApprovalPreview(preview); err != nil {
		t.Fatal(err)
	}
	cloned := capability.CloneApprovalPreview(preview)
	cloned.Summary = "changed"
	cloned.Details[0].Value = "changed"
	if preview.Summary != "Run workspace verification" || preview.Details[0].Value != `["go","test","./..."]` {
		t.Fatalf("CloneApprovalPreview shares storage with input: %#v", preview)
	}
}

func TestApprovalPreviewValidationRejectsUnsafeOrUnboundedContent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		preview *capability.ApprovalPreview
	}{
		{name: "empty summary", preview: &capability.ApprovalPreview{}},
		{name: "control summary", preview: &capability.ApprovalPreview{Summary: "unsafe\x1b[31m"}},
		{name: "format summary", preview: &capability.ApprovalPreview{Summary: "unsafe\u202ereversed"}},
		{name: "invalid summary", preview: &capability.ApprovalPreview{Summary: string([]byte{0xff})}},
		{name: "long summary", preview: &capability.ApprovalPreview{Summary: strings.Repeat("x", 513)}},
		{name: "empty label", preview: &capability.ApprovalPreview{
			Summary: "summary", Details: []capability.ApprovalPreviewDetail{{Value: "value"}},
		}},
		{name: "empty value", preview: &capability.ApprovalPreview{
			Summary: "summary", Details: []capability.ApprovalPreviewDetail{{Label: "label"}},
		}},
		{name: "too many details", preview: &capability.ApprovalPreview{
			Summary: "summary", Details: repeatedApprovalDetails(65),
		}},
		{name: "long value", preview: &capability.ApprovalPreview{
			Summary: "summary",
			Details: []capability.ApprovalPreviewDetail{{Label: "value", Value: strings.Repeat("x", (4<<10)+1)}},
		}},
		{name: "control detail", preview: &capability.ApprovalPreview{
			Summary: "summary",
			Details: []capability.ApprovalPreviewDetail{{Label: "value", Value: "unsafe\nvalue"}},
		}},
		{name: "aggregate size", preview: &capability.ApprovalPreview{
			Summary: strings.Repeat("s", 512),
			Details: []capability.ApprovalPreviewDetail{
				{Label: "first", Value: strings.Repeat("x", 4<<10)},
				{Label: "second", Value: strings.Repeat("y", 2<<10)},
			},
		}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if err := capability.ValidateApprovalPreview(testCase.preview); err == nil {
				t.Fatal("ValidateApprovalPreview succeeded")
			}
		})
	}
}

func TestValidateApprovalArgumentsDigest(t *testing.T) {
	t.Parallel()

	valid := "sha256:" + strings.Repeat("a", 64)
	if err := capability.ValidateApprovalArgumentsDigest(valid); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{
		"",
		"sha256:not-a-digest",
		"sha256:" + strings.Repeat("A", 64),
		"sha512:" + strings.Repeat("a", 64),
	} {
		if err := capability.ValidateApprovalArgumentsDigest(value); err == nil {
			t.Fatalf("ValidateApprovalArgumentsDigest(%q) succeeded", value)
		}
	}
}

func repeatedApprovalDetails(count int) []capability.ApprovalPreviewDetail {
	result := make([]capability.ApprovalPreviewDetail, count)
	for index := range result {
		result[index] = capability.ApprovalPreviewDetail{Label: "label", Value: "value"}
	}
	return result
}
