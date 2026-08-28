package cloudcontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/sivchari/kumo/internal/service/s3"
)

// awsS3Bucket adapts AWS::S3::Bucket to kumo's S3 storage. The Properties
// payload is full-schema (null / empty defaults for what kumo doesn't
// model) so terraform-provider-awscc's "unknown after apply" plan resolves.
type awsS3Bucket struct{}

func init() {
	registerDefaultHandler(&awsS3Bucket{})
}

func (*awsS3Bucket) TypeName() string { return "AWS::S3::Bucket" }

func (*awsS3Bucket) s3Storage() (s3.Storage, error) {
	return lookupStorage[s3.Storage]("s3")
}

// s3BucketProps is the subset of the AWS::S3::Bucket schema kumo maps
// onto real storage. Everything else in the schema is echoed back as a
// static default by s3BucketStateJSON.
type s3BucketProps struct {
	BucketName string        `json:"BucketName"`
	Tags       []s3BucketTag `json:"Tags"`
}

// s3BucketTag is one entry of the resource's Tags property.
type s3BucketTag struct {
	Key   string `json:"Key"`
	Value string `json:"Value"`
}

func (h *awsS3Bucket) Create(ctx context.Context, desiredState []byte) (string, []byte, error) {
	var props s3BucketProps

	if err := json.Unmarshal(desiredState, &props); err != nil {
		return "", nil, fmt.Errorf("invalid AWS::S3::Bucket properties: %w", err)
	}

	if props.BucketName == "" {
		return "", nil, errors.New("BucketName is required")
	}

	storage, err := h.s3Storage()
	if err != nil {
		return "", nil, err
	}

	if err := storage.CreateBucket(ctx, props.BucketName); err != nil {
		return "", nil, err
	}

	if err := applyBucketTags(ctx, storage, props.BucketName, props.Tags); err != nil {
		return "", nil, err
	}

	return props.BucketName, h.stateJSON(ctx, props.BucketName), nil
}

// applyBucketTags writes the resource's Tags property through to the S3
// bucket tag set. An absent or empty Tags list clears the tags, since
// the Cloud Control desired state is always the complete state.
func applyBucketTags(ctx context.Context, storage s3.Storage, bucket string, tags []s3BucketTag) error {
	if len(tags) == 0 {
		return storage.DeleteBucketTagging(ctx, bucket)
	}

	m := make(map[string]string, len(tags))
	for _, t := range tags {
		m[t.Key] = t.Value
	}

	return storage.PutBucketTagging(ctx, bucket, m)
}

func (h *awsS3Bucket) Read(ctx context.Context, identifier string) ([]byte, error) {
	storage, err := h.s3Storage()
	if err != nil {
		return nil, err
	}

	exists, err := storage.BucketExists(ctx, identifier)
	if err != nil {
		return nil, err
	}

	if !exists {
		return nil, &NotFoundError{Message: "bucket " + identifier + " does not exist"}
	}

	return h.stateJSON(ctx, identifier), nil
}

func (h *awsS3Bucket) Update(ctx context.Context, identifier string, patchDocument []byte) ([]byte, error) {
	storage, err := h.s3Storage()
	if err != nil {
		return nil, err
	}

	exists, err := storage.BucketExists(ctx, identifier)
	if err != nil {
		return nil, err
	}

	if !exists {
		return nil, &NotFoundError{Message: "bucket " + identifier + " does not exist"}
	}

	tags, patched, err := tagsFromPatch(patchDocument)
	if err != nil {
		return nil, err
	}

	if patched {
		if err := applyBucketTags(ctx, storage, identifier, tags); err != nil {
			return nil, err
		}
	}

	return h.stateJSON(ctx, identifier), nil
}

// tagsFromPatch reads the resulting Tags list out of an RFC 6902 patch
// document, reporting whether the patch touched /Tags at all. Only whole
// -list operations on /Tags are honoured — the awscc provider always
// rewrites the property as a unit — and operations on any other path are
// ignored, since kumo does not model those properties. A "remove" clears
// the tags, which applyBucketTags handles as an empty list.
func tagsFromPatch(patchDocument []byte) ([]s3BucketTag, bool, error) {
	if len(patchDocument) == 0 {
		return nil, false, nil
	}

	var ops []struct {
		Op    string        `json:"op"`
		Path  string        `json:"path"`
		Value []s3BucketTag `json:"value"`
	}

	if err := json.Unmarshal(patchDocument, &ops); err != nil {
		return nil, false, fmt.Errorf("invalid AWS::S3::Bucket patch document: %w", err)
	}

	var (
		tags    []s3BucketTag
		patched bool
	)

	for _, op := range ops {
		if op.Path != "/Tags" {
			continue
		}

		switch op.Op {
		case "add", "replace":
			tags, patched = op.Value, true
		case "remove":
			tags, patched = nil, true
		}
	}

	return tags, patched, nil
}

// stateJSON renders the resource state for a bucket, filling in the
// Tags property from the live bucket tag set.
func (h *awsS3Bucket) stateJSON(ctx context.Context, name string) []byte {
	var tags []s3BucketTag

	if storage, err := h.s3Storage(); err == nil {
		tags = readBucketTags(ctx, storage, name)
	}

	return s3BucketStateJSON(name, tags)
}

// readBucketTags returns the bucket's tags as resource Tags entries,
// sorted by key so the rendered state is stable. An untagged bucket
// reports NoSuchTagSet, which is not an error here — it is an empty list.
func readBucketTags(ctx context.Context, storage s3.Storage, name string) []s3BucketTag {
	m, err := storage.GetBucketTagging(ctx, name)
	if err != nil {
		return nil
	}

	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	tags := make([]s3BucketTag, 0, len(keys))
	for _, k := range keys {
		tags = append(tags, s3BucketTag{Key: k, Value: m[k]})
	}

	return tags
}

func (h *awsS3Bucket) Delete(ctx context.Context, identifier string) error {
	storage, err := h.s3Storage()
	if err != nil {
		return err
	}

	exists, err := storage.BucketExists(ctx, identifier)
	if err != nil {
		return err
	}

	if !exists {
		return &NotFoundError{Message: "bucket " + identifier + " does not exist"}
	}

	return storage.DeleteBucket(ctx, identifier)
}

func (h *awsS3Bucket) List(ctx context.Context) ([]ResourceDescription, error) {
	storage, err := h.s3Storage()
	if err != nil {
		return nil, err
	}

	buckets, err := storage.ListBuckets(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]ResourceDescription, 0, len(buckets))

	for _, b := range buckets {
		out = append(out, ResourceDescription{
			Identifier: b.Name,
			Properties: s3BucketStateJSON(b.Name, readBucketTags(ctx, storage, b.Name)),
		})
	}

	return out, nil
}

// s3BucketStateJSON emits the full AWS::S3::Bucket CloudFormation schema.
// Sub-resources kumo doesn't model (encryption, lifecycle, replication, …)
// come back as JSON null so the awscc provider's "(known after apply)"
// plan resolves.
func s3BucketStateJSON(name string, tags []s3BucketTag) []byte {
	if tags == nil {
		tags = []s3BucketTag{}
	}

	state := map[string]any{
		"BucketName":                       name,
		"Arn":                              "arn:aws:s3:::" + name,
		"DomainName":                       name + ".s3.amazonaws.com",
		"DualStackDomainName":              name + ".s3.dualstack.us-east-1.amazonaws.com",
		"RegionalDomainName":               name + ".s3.us-east-1.amazonaws.com",
		"WebsiteURL":                       "http://" + name + ".s3-website-us-east-1.amazonaws.com",
		"AccelerateConfiguration":          nil,
		"AccessControl":                    nil,
		"AnalyticsConfigurations":          nil,
		"BucketEncryption":                 nil,
		"CorsConfiguration":                nil,
		"IntelligentTieringConfigurations": nil,
		"InventoryConfigurations":          nil,
		"LifecycleConfiguration":           nil,
		"LoggingConfiguration":             nil,
		"MetadataTableConfiguration":       nil,
		"MetricsConfigurations":            nil,
		"NotificationConfiguration":        nil,
		"ObjectLockConfiguration":          nil,
		"ObjectLockEnabled":                false,
		"OwnershipControls":                nil,
		"PublicAccessBlockConfiguration":   nil,
		"ReplicationConfiguration":         nil,
		"Tags":                             tags,
		"VersioningConfiguration":          nil,
		"WebsiteConfiguration":             nil,
	}

	out, _ := json.Marshal(state)

	return out
}
