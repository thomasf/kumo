package cloudcontrol

import (
	"encoding/json"
	"testing"
)

// TestTagsFromPatch covers how an RFC 6902 patch document is reduced to
// the resulting Tags list.
func TestTagsFromPatch(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		patch       string
		wantTags    []s3BucketTag
		wantPatched bool
	}{
		{
			name:  "empty document leaves tags alone",
			patch: "",
		},
		{
			name:  "patch on another property is ignored",
			patch: `[{"op":"replace","path":"/VersioningConfiguration","value":null}]`,
		},
		{
			name:        "replace sets the whole list",
			patch:       `[{"op":"replace","path":"/Tags","value":[{"Key":"env","Value":"prod"}]}]`,
			wantTags:    []s3BucketTag{{Key: "env", Value: "prod"}},
			wantPatched: true,
		},
		{
			name:        "add sets the whole list",
			patch:       `[{"op":"add","path":"/Tags","value":[{"Key":"team","Value":"platform"}]}]`,
			wantTags:    []s3BucketTag{{Key: "team", Value: "platform"}},
			wantPatched: true,
		},
		{
			name:        "remove clears the list",
			patch:       `[{"op":"remove","path":"/Tags"}]`,
			wantPatched: true,
		},
		{
			name:        "last operation on /Tags wins",
			patch:       `[{"op":"add","path":"/Tags","value":[{"Key":"a","Value":"1"}]},{"op":"remove","path":"/Tags"}]`,
			wantPatched: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tags, patched, err := tagsFromPatch([]byte(tc.patch))
			if err != nil {
				t.Fatalf("tagsFromPatch: %v", err)
			}

			if patched != tc.wantPatched {
				t.Fatalf("patched: got %v, want %v", patched, tc.wantPatched)
			}

			assertTagsEqual(t, tags, tc.wantTags)
		})
	}
}

// assertTagsEqual fails the test unless the two Tags lists match element
// for element, in order.
func assertTagsEqual(t *testing.T, got, want []s3BucketTag) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("tags: got %v, want %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tags[%d]: got %v, want %v", i, got[i], want[i])
		}
	}
}

// TestTagsFromPatch_Malformed confirms a patch document that isn't a JSON
// Patch array is reported rather than silently ignored.
func TestTagsFromPatch_Malformed(t *testing.T) {
	t.Parallel()

	if _, _, err := tagsFromPatch([]byte(`{"Tags":[]}`)); err == nil {
		t.Fatal("expected an error for a non-array patch document, got nil")
	}
}

// TestS3BucketStateJSON_Tags checks the Tags property renders as a JSON
// list — empty rather than null when the bucket has no tags, since the
// awscc provider treats null as "unknown".
func TestS3BucketStateJSON_Tags(t *testing.T) {
	t.Parallel()

	var untagged struct {
		Tags []s3BucketTag `json:"Tags"`
	}

	raw := s3BucketStateJSON("b", nil)
	if err := json.Unmarshal(raw, &untagged); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}

	if untagged.Tags == nil || len(untagged.Tags) != 0 {
		t.Fatalf("untagged bucket Tags: got %v, want []", untagged.Tags)
	}

	var tagged struct {
		Tags []s3BucketTag `json:"Tags"`
	}

	raw = s3BucketStateJSON("b", []s3BucketTag{{Key: "env", Value: "prod"}})
	if err := json.Unmarshal(raw, &tagged); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}

	if len(tagged.Tags) != 1 || tagged.Tags[0] != (s3BucketTag{Key: "env", Value: "prod"}) {
		t.Fatalf("tagged bucket Tags: got %v, want [{env prod}]", tagged.Tags)
	}
}
