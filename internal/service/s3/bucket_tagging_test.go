package s3

import (
	"context"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// tagValueProd is the tag value the round-trip tests write and read back.
const tagValueProd = "prod"

// putBucketTagging drives PUT /{bucket}?tagging with a raw XML body.
func putBucketTagging(svc *Service, bucket, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut, "/"+bucket+"?tagging", strings.NewReader(body))
	req.SetPathValue("bucket", bucket)

	w := httptest.NewRecorder()
	svc.PutBucketTagging(w, req)

	return w
}

// getBucketTagging drives GET /{bucket}?tagging.
func getBucketTagging(svc *Service, bucket string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/"+bucket+"?tagging", http.NoBody)
	req.SetPathValue("bucket", bucket)

	w := httptest.NewRecorder()
	svc.GetBucketTagging(w, req)

	return w
}

// taggingXML wraps tag elements in the document PutBucketTagging expects.
func taggingXML(tags ...string) string {
	return "<Tagging><TagSet>" + strings.Join(tags, "") + "</TagSet></Tagging>"
}

func tagXML(key, value string) string {
	return "<Tag><Key>" + key + "</Key><Value>" + value + "</Value></Tag>"
}

// decodeTagging parses a Tagging response body back into a map.
func decodeTagging(t *testing.T, body string) map[string]string {
	t.Helper()

	var doc Tagging
	if err := xml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("unmarshal Tagging response: %v (body=%s)", err, body)
	}

	tags := make(map[string]string, len(doc.TagSet.Tags))
	for _, tag := range doc.TagSet.Tags {
		tags[tag.Key] = tag.Value
	}

	return tags
}

// TestBucketTagging_RoundTrip covers the PUT → GET → DELETE → GET cycle,
// including the NoSuchTagSet error an untagged bucket reports.
func TestBucketTagging_RoundTrip(t *testing.T) {
	t.Parallel()

	store := NewMemoryStorage()
	svc := New(store, "")
	ctx := context.Background()

	if err := store.CreateBucket(ctx, "tb"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	if w := getBucketTagging(svc, "tb"); w.Code != http.StatusNotFound ||
		!strings.Contains(w.Body.String(), "NoSuchTagSet") {
		t.Fatalf("GET before PUT: got %d %s, want 404 NoSuchTagSet", w.Code, w.Body.String())
	}

	body := taggingXML(tagXML("env", tagValueProd), tagXML("team", "platform"))
	if w := putBucketTagging(svc, "tb", body); w.Code != http.StatusNoContent {
		t.Fatalf("PUT: got %d %s, want 204", w.Code, w.Body.String())
	}

	w := getBucketTagging(svc, "tb")
	if w.Code != http.StatusOK {
		t.Fatalf("GET: got %d %s, want 200", w.Code, w.Body.String())
	}

	got := decodeTagging(t, w.Body.String())
	if got["env"] != tagValueProd || got["team"] != "platform" || len(got) != 2 {
		t.Fatalf("GET tags: got %v, want env=prod team=platform", got)
	}

	// PUT replaces the whole set rather than merging into it.
	if w := putBucketTagging(svc, "tb", taggingXML(tagXML("env", "dev"))); w.Code != http.StatusNoContent {
		t.Fatalf("second PUT: got %d %s, want 204", w.Code, w.Body.String())
	}

	if got := decodeTagging(t, getBucketTagging(svc, "tb").Body.String()); len(got) != 1 || got["env"] != "dev" {
		t.Fatalf("tags after replace: got %v, want only env=dev", got)
	}

	req := httptest.NewRequest(http.MethodDelete, "/tb?tagging", http.NoBody)
	req.SetPathValue("bucket", "tb")

	dw := httptest.NewRecorder()
	svc.DeleteBucketTagging(dw, req)

	if dw.Code != http.StatusNoContent {
		t.Fatalf("DELETE: got %d %s, want 204", dw.Code, dw.Body.String())
	}

	if w := getBucketTagging(svc, "tb"); w.Code != http.StatusNotFound ||
		!strings.Contains(w.Body.String(), "NoSuchTagSet") {
		t.Fatalf("GET after DELETE: got %d %s, want 404 NoSuchTagSet", w.Code, w.Body.String())
	}
}

// TestBucketTagging_MissingBucket confirms bucket-level errors surface as
// NoSuchBucket rather than being reported as an empty tag set.
func TestBucketTagging_MissingBucket(t *testing.T) {
	t.Parallel()

	svc := New(NewMemoryStorage(), "")

	if w := getBucketTagging(svc, "absent"); !strings.Contains(w.Body.String(), "NoSuchBucket") {
		t.Fatalf("GET on missing bucket: got %d %s, want NoSuchBucket", w.Code, w.Body.String())
	}

	w := putBucketTagging(svc, "absent", taggingXML(tagXML("k", "v")))
	if !strings.Contains(w.Body.String(), "NoSuchBucket") {
		t.Fatalf("PUT on missing bucket: got %d %s, want NoSuchBucket", w.Code, w.Body.String())
	}
}

// TestBucketTagging_Validation covers the AWS tag constraints rejected at
// the handler boundary.
func TestBucketTagging_Validation(t *testing.T) {
	t.Parallel()

	store := NewMemoryStorage()
	svc := New(store, "")

	if err := store.CreateBucket(context.Background(), "vb"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	manyTags := make([]string, 0, maxBucketTags+1)
	for i := range maxBucketTags + 1 {
		manyTags = append(manyTags, tagXML(string(rune('a'+i%26))+string(rune('a'+i/26)), "v"))
	}

	cases := []struct {
		name     string
		body     string
		wantCode string
	}{
		{"not XML", "definitely not xml", "MalformedXML"},
		{"empty key", taggingXML(tagXML("", "v")), "InvalidTag"},
		{"duplicate keys", taggingXML(tagXML("k", "a"), tagXML("k", "b")), "InvalidTag"},
		{"key too long", taggingXML(tagXML(strings.Repeat("k", maxTagKeyLength+1), "v")), "InvalidTag"},
		{"value too long", taggingXML(tagXML("k", strings.Repeat("v", maxTagValueLength+1))), "InvalidTag"},
		{"too many tags", taggingXML(manyTags...), "BadRequest"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := putBucketTagging(svc, "vb", tc.body)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), tc.wantCode) {
				t.Fatalf("got %d %s, want 400 %s", w.Code, w.Body.String(), tc.wantCode)
			}
		})
	}

	// None of the rejected requests may have left tags behind.
	if w := getBucketTagging(svc, "vb"); !strings.Contains(w.Body.String(), "NoSuchTagSet") {
		t.Fatalf("bucket gained tags from rejected requests: %s", w.Body.String())
	}
}

// TestBucketTagging_LengthLimitsAreRunes checks the key/value limits count
// Unicode code points, not UTF-8 bytes.
func TestBucketTagging_LengthLimitsAreRunes(t *testing.T) {
	t.Parallel()

	store := NewMemoryStorage()
	svc := New(store, "")

	if err := store.CreateBucket(context.Background(), "rb"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	// 128 runes of a 3-byte character: well over 128 bytes, still legal.
	key := strings.Repeat("あ", maxTagKeyLength)
	if w := putBucketTagging(svc, "rb", taggingXML(tagXML(key, "v"))); w.Code != http.StatusNoContent {
		t.Fatalf("multi-byte key at the limit: got %d %s, want 204", w.Code, w.Body.String())
	}
}

// TestBucketTagging_EmptyTagSetClearsTags confirms an empty TagSet is
// accepted and leaves the bucket reporting NoSuchTagSet, matching S3's
// refusal to distinguish an empty set from an absent one.
func TestBucketTagging_EmptyTagSetClearsTags(t *testing.T) {
	t.Parallel()

	store := NewMemoryStorage()
	svc := New(store, "")

	if err := store.CreateBucket(context.Background(), "eb"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	if w := putBucketTagging(svc, "eb", taggingXML(tagXML("k", "v"))); w.Code != http.StatusNoContent {
		t.Fatalf("seed PUT: got %d %s, want 204", w.Code, w.Body.String())
	}

	if w := putBucketTagging(svc, "eb", taggingXML()); w.Code != http.StatusNoContent {
		t.Fatalf("empty PUT: got %d %s, want 204", w.Code, w.Body.String())
	}

	if w := getBucketTagging(svc, "eb"); !strings.Contains(w.Body.String(), "NoSuchTagSet") {
		t.Fatalf("GET after empty PUT: got %d %s, want NoSuchTagSet", w.Code, w.Body.String())
	}
}

// TestBucketTagging_StoredSetIsCopied guards against the handler's tag map
// aliasing storage, which would let a later mutation leak into the bucket.
func TestBucketTagging_StoredSetIsCopied(t *testing.T) {
	t.Parallel()

	store := NewMemoryStorage()
	ctx := context.Background()

	if err := store.CreateBucket(ctx, "cb"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	in := map[string]string{"env": tagValueProd}
	if err := store.PutBucketTagging(ctx, "cb", in); err != nil {
		t.Fatalf("put tagging: %v", err)
	}

	in["env"] = "mutated"

	out, err := store.GetBucketTagging(ctx, "cb")
	if err != nil {
		t.Fatalf("get tagging: %v", err)
	}

	if out["env"] != tagValueProd {
		t.Fatalf("stored tags aliased the caller's map: got %v", out)
	}

	out["env"] = "mutated-again"

	again, err := store.GetBucketTagging(ctx, "cb")
	if err != nil {
		t.Fatalf("get tagging: %v", err)
	}

	if again["env"] != tagValueProd {
		t.Fatalf("returned tags aliased storage: got %v", again)
	}
}

// TestDeleteObjectTagging covers DELETE /{bucket}/{key}?tagging, including
// the not-found paths.
func TestDeleteObjectTagging(t *testing.T) {
	t.Parallel()

	store := NewMemoryStorage()
	svc := New(store, "")
	ctx := context.Background()

	if err := store.CreateBucket(ctx, "ob"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	if _, err := store.PutObject(ctx, "ob", "k", strings.NewReader("hello"), nil); err != nil {
		t.Fatalf("put object: %v", err)
	}

	if err := store.PutObjectTagging(ctx, "ob", "k", map[string]string{"env": tagValueProd}); err != nil {
		t.Fatalf("put object tagging: %v", err)
	}

	del := func(bucket, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodDelete, "/"+bucket+"/"+key+"?tagging", http.NoBody)
		req.SetPathValue("bucket", bucket)
		req.SetPathValue("key", key)

		w := httptest.NewRecorder()
		svc.DeleteObjectTagging(w, req)

		return w
	}

	if w := del("ob", "k"); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE: got %d %s, want 204", w.Code, w.Body.String())
	}

	tags, err := store.GetObjectTagging(ctx, "ob", "k")
	if err != nil {
		t.Fatalf("get object tagging: %v", err)
	}

	if len(tags) != 0 {
		t.Fatalf("tags after delete: got %v, want none", tags)
	}

	// Deleting again is a no-op, not an error.
	if w := del("ob", "k"); w.Code != http.StatusNoContent {
		t.Fatalf("repeat DELETE: got %d %s, want 204", w.Code, w.Body.String())
	}

	if w := del("ob", "missing"); !strings.Contains(w.Body.String(), "NoSuchKey") {
		t.Fatalf("DELETE on missing key: got %d %s, want NoSuchKey", w.Code, w.Body.String())
	}

	if w := del("absent", "k"); !strings.Contains(w.Body.String(), "NoSuchBucket") {
		t.Fatalf("DELETE on missing bucket: got %d %s, want NoSuchBucket", w.Code, w.Body.String())
	}
}

// TestTaggingDispatch confirms the ?tagging query parameter reaches the
// tagging handlers from each method's dispatcher, rather than falling
// through to CreateBucket / DeleteBucket / ListObjects / DeleteObject.
func TestTaggingDispatch(t *testing.T) {
	t.Parallel()

	store := NewMemoryStorage()
	svc := New(store, "")
	ctx := context.Background()

	if err := store.CreateBucket(ctx, "db"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	if _, err := store.PutObject(ctx, "db", "k", strings.NewReader("hello"), nil); err != nil {
		t.Fatalf("put object: %v", err)
	}

	call := func(method, target, key string, body string, h http.HandlerFunc) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		req.SetPathValue("bucket", "db")

		if key != "" {
			req.SetPathValue("key", key)
		}

		w := httptest.NewRecorder()
		h(w, req)

		return w
	}

	if w := call(http.MethodPut, "/db?tagging", "",
		taggingXML(tagXML("env", tagValueProd)), svc.handleBucketPut); w.Code != http.StatusNoContent {
		t.Fatalf("PUT /db?tagging: got %d %s, want 204", w.Code, w.Body.String())
	}

	w := call(http.MethodGet, "/db?tagging", "", "", svc.handleBucketGet)
	if got := decodeTagging(t, w.Body.String()); got["env"] != tagValueProd {
		t.Fatalf("GET /db?tagging: got %d %s", w.Code, w.Body.String())
	}

	if w := call(http.MethodDelete, "/db?tagging", "", "", svc.handleBucketDelete); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE /db?tagging: got %d %s, want 204", w.Code, w.Body.String())
	}

	// The bucket must have survived: the DELETE hit the tag set, not the bucket.
	if exists, err := store.BucketExists(ctx, "db"); err != nil || !exists {
		t.Fatalf("bucket removed by DELETE ?tagging (exists=%v, err=%v)", exists, err)
	}

	if w := call(http.MethodDelete, "/db/k?tagging", "k", "", svc.handleObjectDelete); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE /db/k?tagging: got %d %s, want 204", w.Code, w.Body.String())
	}

	// Likewise the object must still be there.
	if _, err := store.HeadObject(ctx, "db", "k"); err != nil {
		t.Fatalf("object removed by DELETE ?tagging: %v", err)
	}
}
