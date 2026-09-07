package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	credentialawsworkload "github.com/sparksq/sparkroute/pkg/credentials/awsworkload"
)

func TestSignAWSRequestCanonicalizesBedrockRequest(t *testing.T) {
	t.Parallel()

	body := []byte(`{"messages":[{"role":"user","content":[{"text":"hello"}]}]}`)
	endpoint, err := bedrockOperationURL(
		"https://bedrock-runtime.us-east-1.amazonaws.com",
		"arn:aws:bedrock:us-east-1:123456789012:"+
			"prompt/PROMPT1234:1",
		bedrockOperationConverse,
	)
	if err != nil {
		t.Fatalf("bedrockOperationURL() error = %v", err)
	}
	request, err := http.NewRequest(
		http.MethodPost,
		endpoint,
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	material, _ := json.Marshal(
		credentialawsworkload.SigningMaterial{
			AccessKeyID:     "AKIDEXAMPLE",
			SecretAccessKey: "secret-example",
			SessionToken:    "session-example",
		},
	)
	signingTime := time.Date(
		2026,
		time.July,
		30,
		12,
		34,
		56,
		0,
		time.UTC,
	)
	if err := signAWSRequest(
		request,
		material,
		"us-east-1",
		bedrockAWSService,
		body,
		signingTime,
	); err != nil {
		t.Fatalf("signAWSRequest() error = %v", err)
	}
	digest := sha256.Sum256(body)
	if request.Header.Get("X-Amz-Date") != "20260730T123456Z" ||
		request.Header.Get("X-Amz-Content-Sha256") !=
			hex.EncodeToString(digest[:]) ||
		request.Header.Get("X-Amz-Security-Token") !=
			"session-example" {
		t.Fatalf("signed headers = %#v", request.Header)
	}
	authorization := request.Header.Get("Authorization")
	if !strings.HasPrefix(
		authorization,
		"AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/"+
			"20260730/us-east-1/bedrock/aws4_request, ",
	) ||
		!strings.Contains(
			authorization,
			"SignedHeaders=content-type;host;"+
				"x-amz-content-sha256;x-amz-date;"+
				"x-amz-security-token",
		) ||
		!strings.Contains(authorization, "Signature=") {
		t.Fatalf("Authorization = %q", authorization)
	}
	canonicalURI, err := canonicalAWSURI(request.URL)
	if err != nil {
		t.Fatalf("canonicalAWSURI() error = %v", err)
	}
	if canonicalURI !=
		"/model/arn%253Aaws%253Abedrock%253Aus-east-1%253A"+
			"123456789012%253Aprompt%252FPROMPT1234%253A1/converse" {
		t.Fatalf("canonical URI = %q", canonicalURI)
	}
	if request.URL.EscapedPath() !=
		"/model/arn%3Aaws%3Abedrock%3Aus-east-1%3A"+
			"123456789012%3Aprompt%2FPROMPT1234%3A1/converse" {
		t.Fatalf("wire URI path = %q", request.URL.EscapedPath())
	}
	other, _ := http.NewRequest(
		http.MethodPost,
		request.URL.String(),
		bytes.NewReader([]byte(`{"messages":[]}`)),
	)
	other.Header.Set("Content-Type", "application/json")
	if err := signAWSRequest(
		other,
		material,
		"us-east-1",
		bedrockAWSService,
		[]byte(`{"messages":[]}`),
		signingTime,
	); err != nil {
		t.Fatalf("second sign error = %v", err)
	}
	if other.Header.Get("Authorization") == authorization {
		t.Fatal("signature did not bind the request body")
	}
}

func TestCanonicalAWSQuerySortsAndEncodes(t *testing.T) {
	t.Parallel()

	request, _ := http.NewRequest(
		http.MethodGet,
		"https://example.com/path?z=last&a=space+value&a=%2F",
		nil,
	)
	query, err := canonicalAWSQuery(request.URL)
	if err != nil {
		t.Fatalf("canonicalAWSQuery() error = %v", err)
	}
	if query != "a=%2F&a=space%20value&z=last" {
		t.Fatalf("canonical query = %q", query)
	}
}
