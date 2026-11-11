package ovh

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/ovh/go-ovh/ovh"
	"github.com/ovh/terraform-provider-ovh/v2/ovh/ovhwrap"
	"go.uber.org/ratelimit"
)

const (
	bucketReadTestProject = "/v2/publicCloud/project/p1/storage/object/bucket"
	bucketReadTestId      = "DE_my-bucket"
)

// newBucketReadTestServer fakes the bucket API: the single read answers with single, the list with
// list, and each counts its calls.
// The dummy credentials make go-ovh sign requests, which also asks the server for /auth/time.
func newBucketReadTestServer(t *testing.T, single func(w http.ResponseWriter), list string) (*ovhwrap.Client, *int32, *int32) {
	t.Helper()
	var singleCalls, listCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/time":
			fmt.Fprint(w, "1700000000")
		case bucketReadTestProject + "/" + bucketReadTestId:
			atomic.AddInt32(&singleCalls, 1)
			single(w)
		case bucketReadTestProject:
			atomic.AddInt32(&listCalls, 1)
			fmt.Fprint(w, list)
		default:
			t.Errorf("unexpected request path: %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	client, err := ovh.NewClient(server.URL, "appKey", "appSecret", "consumerKey")
	if err != nil {
		t.Fatalf("failed to create go-ovh client: %s", err)
	}
	return ovhwrap.NewClient(client, ratelimit.NewUnlimited()), &singleCalls, &listCalls
}

func notFound(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNotFound)
	fmt.Fprint(w, `{"class":"Client::NotFound::BucketDoesNotExist","message":"S3 bucket not found"}`)
}

func TestGetStorageObjectBucket_SingleReadAnswers(t *testing.T) {
	client, singleCalls, listCalls := newBucketReadTestServer(t, func(w http.ResponseWriter) {
		fmt.Fprintf(w, `{"id":%q,"resourceStatus":"READY"}`, bucketReadTestId)
	}, `[]`)

	got, err := getStorageObjectBucket(context.Background(), client, "p1", bucketReadTestId)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	// A bucket the single read finds is returned as is, without listing the buckets.
	if got.Id != bucketReadTestId || got.ResourceStatus != "READY" {
		t.Errorf("unexpected bucket: %+v", got)
	}
	if *singleCalls != 1 || *listCalls != 0 {
		t.Errorf("expected 1 single read and no list, got %d and %d", *singleCalls, *listCalls)
	}
}

func TestGetStorageObjectBucket_404ButListed(t *testing.T) {
	client, _, listCalls := newBucketReadTestServer(t, notFound,
		fmt.Sprintf(`[{"id":"DE_other","resourceStatus":"READY"},{"id":%q,"resourceStatus":"READY","targetSpec":{"name":"my-bucket"}}]`, bucketReadTestId))

	got, err := getStorageObjectBucket(context.Background(), client, "p1", bucketReadTestId)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	// The spurious 404 falls back to the list, which yields this bucket and not its neighbor.
	if got.Id != bucketReadTestId || got.TargetSpec == nil || got.TargetSpec.Name != "my-bucket" {
		t.Errorf("unexpected bucket: %+v", got)
	}
	if *listCalls != 1 {
		t.Errorf("expected 1 list call, got %d", *listCalls)
	}
}

func TestGetStorageObjectBucket_404AndNotListed(t *testing.T) {
	client, _, _ := newBucketReadTestServer(t, notFound, `[{"id":"DE_other","resourceStatus":"READY"}]`)

	_, err := getStorageObjectBucket(context.Background(), client, "p1", bucketReadTestId)

	// A bucket missing from both is gone, reported as the single read's 404, which Read turns into
	// removing it from the state.
	errOvh, ok := err.(*ovh.APIError)
	if !ok || errOvh.Code != http.StatusNotFound {
		t.Fatalf("expected the single read's 404 *ovh.APIError, got %T: %v", err, err)
	}
}

func TestGetStorageObjectBucket_OtherErrorsSkipTheList(t *testing.T) {
	client, _, listCalls := newBucketReadTestServer(t, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"class":"Server::InternalServerError","message":"Internal server error"}`)
	}, `[]`)

	_, err := getStorageObjectBucket(context.Background(), client, "p1", bucketReadTestId)

	// Only a 404 is double-checked; any other failure surfaces, and is no 404 that would drop the
	// bucket from the state.
	if err == nil {
		t.Fatal("expected an error")
	}
	if errOvh, ok := err.(*ovh.APIError); ok && errOvh.Code == http.StatusNotFound {
		t.Errorf("a 500 must not surface as a 404: %v", err)
	}
	if *listCalls != 0 {
		t.Errorf("expected no list call, got %d", *listCalls)
	}
}
