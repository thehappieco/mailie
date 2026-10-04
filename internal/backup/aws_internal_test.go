package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

const testKeyARN = "arn:aws:kms:us-east-2:111122223333:key/00000000-0000-4000-8000-000000000001"

type seenRequest struct {
	method, path string
	header       http.Header
	body         []byte
}

// recorder is a local stand-in for an AWS endpoint that records every
// request and answers each with respond.
func recorder(t *testing.T, respond func(w http.ResponseWriter, r seenRequest)) (*httptest.Server, func() []seenRequest) {
	t.Helper()
	var mu sync.Mutex
	var seen []seenRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		req := seenRequest{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: body}
		mu.Lock()
		seen = append(seen, req)
		mu.Unlock()
		respond(w, req)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []seenRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]seenRequest(nil), seen...)
	}
}

func testAWS(kmsURL, s3URL string) *AWS {
	cfg := aws.Config{
		Region: "us-east-2",
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test-only", SecretAccessKey: "test-only"}, nil
		}),
	}
	return newAWS(cfg,
		[]func(*kms.Options){func(o *kms.Options) { o.BaseEndpoint = aws.String(kmsURL) }},
		[]func(*s3.Options){func(o *s3.Options) { o.BaseEndpoint = aws.String(s3URL); o.UsePathStyle = true }},
	)
}

func TestTheUploadIsOneConditionalPutThatS3EncryptsWithItsOwnKey(t *testing.T) {
	srv, seen := recorder(t, func(w http.ResponseWriter, _ seenRequest) {
		w.Header().Set("ETag", `"x"`)
		w.WriteHeader(http.StatusOK)
	})
	a := testAWS("http://unused.invalid", srv.URL)
	body := bytes.Repeat([]byte("sealed backup bytes "), 1000)
	sum := sha256.Sum256(body)
	err := a.PutObject(context.Background(), PutInput{
		Bucket: "example-mail-backups", Key: "db/20261001T000000Z-00000000.mlbk",
		Body: bytes.NewReader(body), Size: int64(len(body)), SHA256: sum[:],
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	reqs := seen()
	if len(reqs) != 1 {
		t.Fatalf("%d requests; the host needs one PutObject and nothing else", len(reqs))
	}
	r := reqs[0]
	if r.method != http.MethodPut || r.path != "/example-mail-backups/db/20261001T000000Z-00000000.mlbk" {
		t.Fatalf("%s %s", r.method, r.path)
	}
	for name, want := range map[string]string{
		// No overwrite: a bucket policy can refuse the host a PutObject without it.
		"If-None-Match": "*",
		// At rest, SSE-S3. S3's own call to KMS for SSE-KMS would carry the
		// context aws:s3:arn, which a backup key's policy refuses.
		"X-Amz-Server-Side-Encryption": "AES256",
		// S3 checks what arrived against the digest computed on the host.
		"X-Amz-Checksum-Sha256": base64.StdEncoding.EncodeToString(sum[:]),
	} {
		if got := r.header.Get(name); got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
	for _, name := range []string{
		"X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id",
		"X-Amz-Server-Side-Encryption-Bucket-Key-Enabled",
		"X-Amz-Server-Side-Encryption-Context",
	} {
		if got, ok := r.header[name]; ok {
			t.Errorf("%s is sent (%q): the upload names no KMS key", name, got)
		}
	}
	if r.header.Get("Content-Encoding") == "aws-chunked" {
		t.Fatal("the body went aws-chunked; the test would need to decode it")
	}
	if !bytes.Equal(r.body, body) {
		t.Fatalf("the body arrived as %d bytes, want the %d sent", len(r.body), len(body))
	}
}

func TestKMSIsAskedForThisKeyWithThisContext(t *testing.T) {
	encCtx := EncryptionContext("prod", "db/20261001T000000Z-00000000.mlbk")
	plain := bytes.Repeat([]byte{7}, 32)
	srv, seen := recorder(t, func(w http.ResponseWriter, r seenRequest) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		out := map[string]any{"KeyId": testKeyARN, "Plaintext": plain}
		if strings.HasSuffix(r.header.Get("X-Amz-Target"), ".GenerateDataKey") {
			out["CiphertextBlob"] = []byte("wrapped")
		}
		if err := json.NewEncoder(w).Encode(out); err != nil {
			t.Error(err)
		}
	})
	a := testAWS(srv.URL, "http://unused.invalid")
	ctx := context.Background()

	dk, err := a.GenerateDataKey(ctx, testKeyARN, encCtx)
	if err != nil {
		t.Fatalf("GenerateDataKey: %v", err)
	}
	if dk.KeyARN != testKeyARN || !bytes.Equal(dk.Plaintext, plain) || string(dk.Wrapped) != "wrapped" {
		t.Fatalf("data key %+v", dk)
	}
	if _, err := a.Decrypt(ctx, testKeyARN, []byte("wrapped"), encCtx); err != nil {
		t.Fatalf("Decrypt: %v", err)
	}

	reqs := seen()
	if len(reqs) != 2 {
		t.Fatalf("%d requests", len(reqs))
	}
	for i, want := range []struct {
		target string
		fields map[string]any
	}{
		{"TrentService.GenerateDataKey", map[string]any{"KeyId": testKeyARN, "KeySpec": "AES_256"}},
		{"TrentService.Decrypt", map[string]any{
			"KeyId": testKeyARN, "EncryptionAlgorithm": "SYMMETRIC_DEFAULT",
			"CiphertextBlob": base64.StdEncoding.EncodeToString([]byte("wrapped")),
		}},
	} {
		if got := reqs[i].header.Get("X-Amz-Target"); got != want.target {
			t.Fatalf("request %d is %s, want %s", i, got, want.target)
		}
		var body map[string]any
		if err := json.Unmarshal(reqs[i].body, &body); err != nil {
			t.Fatal(err)
		}
		for k, v := range want.fields {
			if body[k] != v {
				t.Errorf("%s %s = %v, want %v", want.target, k, body[k], v)
			}
		}
		got := map[string]string{}
		for k, v := range body["EncryptionContext"].(map[string]any) {
			got[k], _ = v.(string)
		}
		if !maps.Equal(got, encCtx) {
			t.Errorf("%s context %v, want %v", want.target, got, encCtx)
		}
	}
}

func TestADecryptAnsweredForAnotherKeyIsRefused(t *testing.T) {
	plain := bytes.Repeat([]byte{7}, 32)
	srv, _ := recorder(t, func(w http.ResponseWriter, _ seenRequest) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		other := strings.Replace(testKeyARN, "0001", "0002", 1)
		if err := json.NewEncoder(w).Encode(map[string]any{"KeyId": other, "Plaintext": plain}); err != nil {
			t.Error(err)
		}
	})
	a := testAWS(srv.URL, "http://unused.invalid")
	if _, err := a.Decrypt(context.Background(), testKeyARN, []byte("wrapped"), nil); err == nil {
		t.Fatal("a data key unwrapped by another key was accepted")
	}
}
