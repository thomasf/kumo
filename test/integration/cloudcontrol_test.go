//go:build integration

package integration

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudcontrol"
	"github.com/aws/aws-sdk-go-v2/service/cloudcontrol/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/sivchari/golden"
)

func newCloudControlClient(t *testing.T) *cloudcontrol.Client {
	t.Helper()

	return cloudcontrol.NewFromConfig(awsConfig(t), func(o *cloudcontrol.Options) {
		o.BaseEndpoint = aws.String(testEndpoint())
	})
}

func TestCloudControl_S3BucketLifecycle(t *testing.T) {
	client := newCloudControlClient(t)
	ctx := t.Context()
	typeName := "AWS::S3::Bucket"
	bucketName := "cloudcontrol-integration-bucket"
	desiredState := `{"BucketName":"cloudcontrol-integration-bucket"}`

	_, _ = client.DeleteResource(context.Background(), &cloudcontrol.DeleteResourceInput{
		TypeName:   aws.String(typeName),
		Identifier: aws.String(bucketName),
	})

	t.Cleanup(func() {
		_, _ = client.DeleteResource(context.Background(), &cloudcontrol.DeleteResourceInput{
			TypeName:   aws.String(typeName),
			Identifier: aws.String(bucketName),
		})
	})

	g := golden.New(t)

	createOutput, err := client.CreateResource(ctx, &cloudcontrol.CreateResourceInput{
		TypeName:     aws.String(typeName),
		DesiredState: aws.String(desiredState),
		ClientToken:  aws.String("cloudcontrol-s3-create"),
	})
	if err != nil {
		t.Fatalf("CreateResource: %v", err)
	}

	if got := aws.ToString(createOutput.ProgressEvent.Identifier); got != bucketName {
		t.Fatalf("CreateResource identifier = %q, want %q", got, bucketName)
	}

	g.Assert(t.Name()+"/create", stableCloudControlProgressEvent(createOutput.ProgressEvent))

	statusOutput, err := client.GetResourceRequestStatus(ctx, &cloudcontrol.GetResourceRequestStatusInput{
		RequestToken: createOutput.ProgressEvent.RequestToken,
	})
	if err != nil {
		t.Fatalf("GetResourceRequestStatus: %v", err)
	}

	g.Assert(t.Name()+"/status", stableCloudControlProgressEvent(statusOutput.ProgressEvent))

	getOutput, err := client.GetResource(ctx, &cloudcontrol.GetResourceInput{
		TypeName:   aws.String(typeName),
		Identifier: aws.String(bucketName),
	})
	if err != nil {
		t.Fatalf("GetResource: %v", err)
	}

	g.Assert(t.Name()+"/get", stableCloudControlResourceDescription(
		aws.ToString(getOutput.TypeName),
		getOutput.ResourceDescription,
	))

	listOutput, err := client.ListResources(ctx, &cloudcontrol.ListResourcesInput{
		TypeName: aws.String(typeName),
	})
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}

	resource := findCloudControlResource(t, listOutput.ResourceDescriptions, bucketName)
	g.Assert(t.Name()+"/list", stableCloudControlResourceDescription("", &resource))

	deleteOutput, err := client.DeleteResource(ctx, &cloudcontrol.DeleteResourceInput{
		TypeName:    aws.String(typeName),
		Identifier:  aws.String(bucketName),
		ClientToken: aws.String("cloudcontrol-s3-delete"),
	})
	if err != nil {
		t.Fatalf("DeleteResource: %v", err)
	}

	g.Assert(t.Name()+"/delete", stableCloudControlProgressEvent(deleteOutput.ProgressEvent))

	_, err = client.GetResource(ctx, &cloudcontrol.GetResourceInput{
		TypeName:   aws.String(typeName),
		Identifier: aws.String(bucketName),
	})
	if err == nil {
		t.Fatalf("GetResource after delete error = nil")
	}

	var notFound *types.ResourceNotFoundException
	if !errors.As(err, &notFound) {
		t.Fatalf("GetResource after delete error = %T: %v, want ResourceNotFoundException", err, err)
	}
}

func findCloudControlResource(
	t *testing.T,
	resources []types.ResourceDescription,
	identifier string,
) types.ResourceDescription {
	t.Helper()

	for _, resource := range resources {
		if aws.ToString(resource.Identifier) == identifier {
			return resource
		}
	}

	t.Fatalf("resource %q not found in ListResources", identifier)

	return types.ResourceDescription{}
}

func stableCloudControlProgressEvent(event *types.ProgressEvent) map[string]any {
	if event == nil {
		return nil
	}

	return map[string]any{
		"ErrorCode":       string(event.ErrorCode),
		"Identifier":      aws.ToString(event.Identifier),
		"Operation":       string(event.Operation),
		"OperationStatus": string(event.OperationStatus),
		"RequestToken":    aws.ToString(event.RequestToken),
		"ResourceModel":   aws.ToString(event.ResourceModel),
		"StatusMessage":   aws.ToString(event.StatusMessage),
		"TypeName":        aws.ToString(event.TypeName),
	}
}

func stableCloudControlResourceDescription(
	typeName string,
	resource *types.ResourceDescription,
) map[string]any {
	if resource == nil {
		return nil
	}

	out := map[string]any{
		"Identifier": aws.ToString(resource.Identifier),
		"Properties": aws.ToString(resource.Properties),
	}

	if typeName != "" {
		out["TypeName"] = typeName
	}

	return out
}

// TestCloudControl_S3BucketTags checks that the resource's Tags property
// is backed by the real S3 bucket tag set: set on create, visible through
// GetResource, and rewritten by an UpdateResource patch.
func TestCloudControl_S3BucketTags(t *testing.T) {
	client := newCloudControlClient(t)
	s3Client := newS3Client(t)
	ctx := t.Context()
	typeName := "AWS::S3::Bucket"
	bucketName := "cloudcontrol-tagged-bucket"

	_, _ = client.DeleteResource(context.Background(), &cloudcontrol.DeleteResourceInput{
		TypeName:   aws.String(typeName),
		Identifier: aws.String(bucketName),
	})

	t.Cleanup(func() {
		_, _ = client.DeleteResource(context.Background(), &cloudcontrol.DeleteResourceInput{
			TypeName:   aws.String(typeName),
			Identifier: aws.String(bucketName),
		})
	})

	_, err := client.CreateResource(ctx, &cloudcontrol.CreateResourceInput{
		TypeName: aws.String(typeName),
		DesiredState: aws.String(
			`{"BucketName":"cloudcontrol-tagged-bucket","Tags":[{"Key":"env","Value":"prod"}]}`,
		),
		ClientToken: aws.String("cloudcontrol-s3-tags-create"),
	})
	if err != nil {
		t.Fatalf("CreateResource: %v", err)
	}

	// The tags must be readable through the S3 API, not just echoed back.
	tagging, err := s3Client.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: aws.String(bucketName)})
	if err != nil {
		t.Fatalf("GetBucketTagging after create: %v", err)
	}

	if len(tagging.TagSet) != 1 || aws.ToString(tagging.TagSet[0].Key) != "env" ||
		aws.ToString(tagging.TagSet[0].Value) != "prod" {
		t.Fatalf("bucket tags after create = %v, want env=prod", tagging.TagSet)
	}

	getOutput, err := client.GetResource(ctx, &cloudcontrol.GetResourceInput{
		TypeName:   aws.String(typeName),
		Identifier: aws.String(bucketName),
	})
	if err != nil {
		t.Fatalf("GetResource: %v", err)
	}

	if props := aws.ToString(getOutput.ResourceDescription.Properties); !strings.Contains(
		props, `"Tags":[{"Key":"env","Value":"prod"}]`,
	) {
		t.Fatalf("GetResource properties missing the bucket tags: %s", props)
	}

	_, err = client.UpdateResource(ctx, &cloudcontrol.UpdateResourceInput{
		TypeName:      aws.String(typeName),
		Identifier:    aws.String(bucketName),
		PatchDocument: aws.String(`[{"op":"replace","path":"/Tags","value":[{"Key":"env","Value":"staging"}]}]`),
		ClientToken:   aws.String("cloudcontrol-s3-tags-update"),
	})
	if err != nil {
		t.Fatalf("UpdateResource: %v", err)
	}

	tagging, err = s3Client.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: aws.String(bucketName)})
	if err != nil {
		t.Fatalf("GetBucketTagging after update: %v", err)
	}

	if len(tagging.TagSet) != 1 || aws.ToString(tagging.TagSet[0].Value) != "staging" {
		t.Fatalf("bucket tags after update = %v, want env=staging", tagging.TagSet)
	}
}
