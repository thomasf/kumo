package s3

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"maps"
	"net/http"
	"sort"
	"unicode/utf8"
)

// AWS tag constraints applied to a bucket's TagSet. Keys are 1-128
// characters, values 0-256, and a bucket carries at most 50 tags. The
// limits are in Unicode code points, not bytes, which is why the checks
// use utf8.RuneCountInString.
const (
	maxTagKeyLength   = 128
	maxTagValueLength = 256
	maxBucketTags     = 50
)

// PutBucketTagging handles PUT /{bucket}?tagging.
func (s *Service) PutBucketTagging(w http.ResponseWriter, r *http.Request) {
	bucket := r.PathValue("bucket")

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeS3Error(w, r, "InvalidRequest", "Failed to read request body", http.StatusBadRequest)

		return
	}

	var tagging Tagging
	if err := xml.Unmarshal(body, &tagging); err != nil {
		writeS3Error(w, r, "MalformedXML", fmt.Sprintf("Tagging XML: %v", err), http.StatusBadRequest)

		return
	}

	tags, tagErr := tagMapFromTagSet(tagging.TagSet.Tags, maxBucketTags)
	if tagErr != nil {
		writeS3Error(w, r, tagErr.Code, tagErr.Message, http.StatusBadRequest)

		return
	}

	if err := s.storage.PutBucketTagging(r.Context(), bucket, tags); err != nil {
		handleBucketLevelError(w, r, err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// GetBucketTagging handles GET /{bucket}?tagging. A bucket with no tags
// is not an empty TagSet in S3 — it is the NoSuchTagSet error, which the
// storage layer reports.
func (s *Service) GetBucketTagging(w http.ResponseWriter, r *http.Request) {
	bucket := r.PathValue("bucket")

	tags, err := s.storage.GetBucketTagging(r.Context(), bucket)
	if err != nil {
		handleBucketLevelError(w, r, err)

		return
	}

	tagging := taggingFromMap(tags)
	tagging.Xmlns = s3Namespace
	writeXMLResponse(w, tagging)
}

// DeleteBucketTagging handles DELETE /{bucket}?tagging.
func (s *Service) DeleteBucketTagging(w http.ResponseWriter, r *http.Request) {
	bucket := r.PathValue("bucket")

	if err := s.storage.DeleteBucketTagging(r.Context(), bucket); err != nil {
		handleBucketLevelError(w, r, err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// tagError is a tag validation failure carrying the S3 error code and
// message to report. All of them are 400s.
type tagError struct {
	Code    string
	Message string
}

func (e *tagError) Error() string {
	return e.Code + ": " + e.Message
}

// tagMapFromTagSet validates a parsed TagSet against the AWS tag rules
// and collapses it to a map. Duplicate keys are rejected rather than
// silently overwritten, matching S3.
func tagMapFromTagSet(tagList []Tag, maxTags int) (map[string]string, *tagError) {
	if len(tagList) > maxTags {
		return nil, &tagError{
			Code:    "BadRequest",
			Message: fmt.Sprintf("Bucket tag count cannot be greater than %d", maxTags),
		}
	}

	tags := make(map[string]string, len(tagList))

	for _, tag := range tagList {
		if err := validateTag(tag); err != nil {
			return nil, err
		}

		if _, dup := tags[tag.Key]; dup {
			return nil, &tagError{Code: "InvalidTag", Message: "Cannot provide multiple Tags with the same key"}
		}

		tags[tag.Key] = tag.Value
	}

	return tags, nil
}

// validateTag checks a single tag's key and value against the AWS limits.
func validateTag(tag Tag) *tagError {
	keyLen := utf8.RuneCountInString(tag.Key)

	switch {
	case keyLen == 0:
		return &tagError{Code: "InvalidTag", Message: "The TagKey you have provided is invalid"}
	case keyLen > maxTagKeyLength:
		return &tagError{
			Code:    "InvalidTag",
			Message: fmt.Sprintf("The TagKey provided is too long, %d", keyLen),
		}
	}

	if valueLen := utf8.RuneCountInString(tag.Value); valueLen > maxTagValueLength {
		return &tagError{
			Code:    "InvalidTag",
			Message: fmt.Sprintf("The TagValue provided is too long, %d", valueLen),
		}
	}

	return nil
}

// taggingFromMap builds the Tagging XML document for a tag map, with
// tags sorted by key so responses are byte-stable across requests.
func taggingFromMap(tags map[string]string) Tagging {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	tagging := Tagging{TagSet: TagSet{Tags: make([]Tag, 0, len(keys))}}
	for _, k := range keys {
		tagging.TagSet.Tags = append(tagging.TagSet.Tags, Tag{Key: k, Value: tags[k]})
	}

	return tagging
}

// MemoryStorage hooks for bucket tagging.

// PutBucketTagging replaces the bucket's tag set. S3 has no partial
// update: the request body is the complete set.
func (s *MemoryStorage) PutBucketTagging(_ context.Context, bucket string, tags map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	b, ok := s.Buckets[bucket]
	if !ok {
		return &BucketError{Code: "NoSuchBucket", Message: "The specified bucket does not exist", BucketName: bucket}
	}

	b.Tags = maps.Clone(tags)

	s.saveLocked()

	return nil
}

// GetBucketTagging returns a copy of the bucket's tag set, or
// NoSuchTagSet when the bucket has no tags. An empty tag set is
// indistinguishable from an absent one, which is how S3 behaves.
func (s *MemoryStorage) GetBucketTagging(_ context.Context, bucket string) (map[string]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	b, ok := s.Buckets[bucket]
	if !ok {
		return nil, &BucketError{Code: "NoSuchBucket", Message: "The specified bucket does not exist", BucketName: bucket}
	}

	if len(b.Tags) == 0 {
		return nil, &BucketError{Code: "NoSuchTagSet", Message: "The TagSet does not exist", BucketName: bucket}
	}

	return maps.Clone(b.Tags), nil
}

// DeleteBucketTagging removes every tag from the bucket. Idempotent —
// deleting from an untagged bucket is a no-op.
func (s *MemoryStorage) DeleteBucketTagging(_ context.Context, bucket string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	b, ok := s.Buckets[bucket]
	if !ok {
		return &BucketError{Code: "NoSuchBucket", Message: "The specified bucket does not exist", BucketName: bucket}
	}

	b.Tags = nil

	s.saveLocked()

	return nil
}
