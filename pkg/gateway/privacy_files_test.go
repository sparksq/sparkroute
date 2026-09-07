package gateway

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"testing"

	"github.com/sparksq/sparkroute/pkg/config"
)

func TestPreparePIIFileUploadInspectsOnlyTypedTextAndMetadata(t *testing.T) {
	session := deterministicPIISession(t)
	policy := (&config.PIIPolicy{
		Scope: config.PIIScopeConversation,
		Files: &config.PIIFilePolicy{
			Text: config.PIIFileTextRequired, Metadata: true,
		},
	}).Effective()
	privacyContext := filePIIContext{policy: policy, session: session}

	var upload bytes.Buffer
	writer := multipart.NewWriter(&upload)
	if err := writer.WriteField("purpose", "contact alice@example.com"); err != nil {
		t.Fatal(err)
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="file"; filename="alice@example.com"`)
	header.Set("Content-Type", "text/plain; charset=utf-8")
	part, err := writer.CreatePart(header)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(part, "email alice@example.com")
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	masked, contentType, err := preparePIIFileUpload(
		t.Context(), upload.Bytes(), writer.FormDataContentType(), privacyContext, 1<<20,
	)
	if err != nil {
		t.Fatalf("preparePIIFileUpload() error = %v", err)
	}
	request, _ := http.NewRequest(http.MethodPost, "/", bytes.NewReader(masked))
	request.Header.Set("Content-Type", contentType)
	if err := request.ParseMultipartForm(1 << 20); err != nil {
		t.Fatal(err)
	}
	file, fileHeader, err := request.FormFile("file")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	body, _ := io.ReadAll(file)
	for name, value := range map[string]string{
		"filename": fileHeader.Filename,
		"purpose":  request.FormValue("purpose"),
		"content":  string(body),
	} {
		if strings.Contains(value, "alice@example.com") ||
			!strings.Contains(value, "[[SPARKROUTE_PII_EMAIL_TOKEN]]") {
			t.Fatalf("%s was not substituted: %q", name, value)
		}
	}

	caller, err := transformFileTextResponse(
		t.Context(), []byte("known [[SPARKROUTE_PII_EMAIL_TOKEN]] novel bob@example.com"),
		privacyContext,
	)
	if err != nil || string(caller) != "known alice@example.com novel [[SPARKROUTE_PII_EMAIL_REDACTED]]" {
		t.Fatalf("transformFileTextResponse() = %q, %v", caller, err)
	}
}

func TestPreparePIIFileUploadRequiredRejectsBinary(t *testing.T) {
	policy := (&config.PIIPolicy{
		Scope: config.PIIScopeConversation,
		Files: &config.PIIFilePolicy{Text: config.PIIFileTextRequired},
	}).Effective()
	var upload bytes.Buffer
	writer := multipart.NewWriter(&upload)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="file"; filename="image.png"`)
	header.Set("Content-Type", "image/png")
	part, _ := writer.CreatePart(header)
	_, _ = part.Write([]byte{0x89, 'P', 'N', 'G'})
	_ = writer.Close()

	_, _, err := preparePIIFileUpload(
		t.Context(), upload.Bytes(), writer.FormDataContentType(),
		filePIIContext{policy: policy, session: deterministicPIISession(t)}, 1<<20,
	)
	if err == nil || !strings.Contains(err.Error(), "only explicitly typed text") {
		t.Fatalf("preparePIIFileUpload() error = %v", err)
	}
}
