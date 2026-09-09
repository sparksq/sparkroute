// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/textproto"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/identity"
	"github.com/sparksq/sparkroute/pkg/privacy"
	"github.com/sparksq/sparkroute/pkg/responsesstate"
)

const maximumMultipartPIIParts = 128

type filePIIContext struct {
	policy  config.EffectivePIIPolicy
	session privacy.Session
}

func (c filePIIContext) enabled() bool {
	return c.session != nil &&
		(c.policy.Files.Text != config.PIIFileTextDisabled || c.policy.Files.Metadata)
}

func (h *filesHandler) privacyContext(
	ctx context.Context,
	modelName string,
	caller identity.Identity,
) (filePIIContext, error) {
	model, err := h.core.snapshot.ResolveModel(modelName, false)
	if err != nil || model.Privacy == nil {
		return filePIIContext{}, nil
	}
	policy := model.Privacy.PII.Effective()
	if policy.Mode == config.PIIModeDisabled ||
		(policy.Files.Text == config.PIIFileTextDisabled && !policy.Files.Metadata) {
		return filePIIContext{}, nil
	}
	session, err := newPIISession(ctx, h.core.privacy, policy, caller)
	if err != nil {
		recordPrivacyFailure(h.core.privacy)
		return filePIIContext{policy: policy}, err
	}
	recordPrivacyInspection(h.core.privacy)
	return filePIIContext{policy: policy, session: session}, nil
}

func preparePIIFileUpload(
	ctx context.Context,
	raw []byte,
	contentType string,
	privacyContext filePIIContext,
	maximumRequestBytes int64,
) ([]byte, string, error) {
	mediaType, parameters, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "multipart/form-data" || parameters["boundary"] == "" {
		return nil, "", fmt.Errorf("multipart upload content type is invalid")
	}
	reader := multipart.NewReader(bytes.NewReader(raw), parameters["boundary"])
	var output bytes.Buffer
	writer := multipart.NewWriter(&output)
	if err := writer.SetBoundary(parameters["boundary"]); err != nil {
		return nil, "", fmt.Errorf("preserve multipart boundary: %w", err)
	}
	parts := 0
	for {
		part, nextErr := reader.NextPart()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return nil, "", fmt.Errorf("read multipart upload: %w", nextErr)
		}
		parts++
		if parts > maximumMultipartPIIParts {
			_ = part.Close()
			return nil, "", fmt.Errorf("multipart upload contains too many parts")
		}
		payload, readErr := io.ReadAll(io.LimitReader(part, maximumRequestBytes+1))
		if closeErr := part.Close(); readErr == nil {
			readErr = closeErr
		}
		if readErr != nil || int64(len(payload)) > maximumRequestBytes {
			return nil, "", fmt.Errorf("multipart upload part exceeds the request limit")
		}
		header := cloneMIMEHeader(part.Header)
		filename := part.FileName()
		inspectionFilename := filename
		if privacyContext.policy.Files.Metadata && filename != "" {
			filename, err = privacyContext.session.Substitute(ctx, filename)
			if err != nil {
				return nil, "", err
			}
			if err := replaceMultipartFilename(header, filename); err != nil {
				return nil, "", err
			}
		}
		if privacyContext.policy.Files.Metadata && part.FormName() == "purpose" && filename == "" {
			value, transformErr := privacyContext.session.Substitute(ctx, string(payload))
			if transformErr != nil {
				return nil, "", transformErr
			}
			payload = []byte(value)
		}
		if part.FormName() == "file" && filename != "" &&
			privacyContext.policy.Files.Text != config.PIIFileTextDisabled {
			textual := inspectableTextUpload(header.Get("Content-Type"), inspectionFilename)
			required := privacyContext.policy.Files.Text == config.PIIFileTextRequired
			switch {
			case !textual && required:
				return nil, "", fmt.Errorf("required PII inspection accepts only explicitly typed text files")
			case textual && int64(len(payload)) > privacyContext.policy.Files.MaxTextBytes && required:
				return nil, "", fmt.Errorf("text file exceeds the configured PII inspection limit")
			case textual && !utf8.Valid(payload) && required:
				return nil, "", fmt.Errorf("text file is not valid UTF-8")
			case textual && int64(len(payload)) <= privacyContext.policy.Files.MaxTextBytes && utf8.Valid(payload):
				value, transformErr := privacyContext.session.Substitute(ctx, string(payload))
				if transformErr != nil {
					return nil, "", transformErr
				}
				payload = []byte(value)
			}
		}
		destination, err := writer.CreatePart(header)
		if err != nil {
			return nil, "", fmt.Errorf("rebuild multipart upload: %w", err)
		}
		if _, err := destination.Write(payload); err != nil {
			return nil, "", fmt.Errorf("rebuild multipart upload payload: %w", err)
		}
		if int64(output.Len()) > maximumRequestBytes {
			return nil, "", fmt.Errorf("PII-transformed multipart upload exceeds the request limit")
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", fmt.Errorf("finish PII-transformed multipart upload: %w", err)
	}
	if int64(output.Len()) > maximumRequestBytes {
		return nil, "", fmt.Errorf("PII-transformed multipart upload exceeds the request limit")
	}
	return output.Bytes(), writer.FormDataContentType(), nil
}

func cloneMIMEHeader(source textproto.MIMEHeader) textproto.MIMEHeader {
	result := make(textproto.MIMEHeader, len(source))
	for name, values := range source {
		result[name] = append([]string(nil), values...)
	}
	return result
}

func replaceMultipartFilename(header textproto.MIMEHeader, filename string) error {
	disposition, parameters, err := mime.ParseMediaType(header.Get("Content-Disposition"))
	if err != nil || disposition != "form-data" {
		return fmt.Errorf("multipart file disposition is invalid")
	}
	parameters["filename"] = filename
	header.Set("Content-Disposition", mime.FormatMediaType(disposition, parameters))
	return nil
}

func inspectableTextUpload(contentType string, filename string) bool {
	mediaType := ""
	if strings.TrimSpace(contentType) != "" {
		parsed, _, err := mime.ParseMediaType(contentType)
		if err != nil {
			return false
		}
		mediaType = strings.ToLower(parsed)
	}
	if mediaType == "" || mediaType == "application/octet-stream" {
		mediaType = strings.ToLower(strings.TrimSpace(mime.TypeByExtension(
			strings.ToLower(filepath.Ext(filename)),
		)))
		if parsed, _, err := mime.ParseMediaType(mediaType); err == nil {
			mediaType = parsed
		}
	}
	if strings.HasPrefix(mediaType, "text/") {
		return true
	}
	switch mediaType {
	case "application/json", "application/ld+json", "application/x-ndjson",
		"application/xml", "application/yaml", "application/x-yaml",
		"application/javascript", "application/x-www-form-urlencoded",
		"application/sql", "application/graphql":
		return true
	default:
		return strings.HasSuffix(mediaType, "+json") || strings.HasSuffix(mediaType, "+xml")
	}
}

func transformFileMetadataResponse(
	ctx context.Context,
	payload []byte,
	privacyContext filePIIContext,
) ([]byte, error) {
	var document any
	if err := json.Unmarshal(payload, &document); err != nil {
		return nil, err
	}
	transform := func(value string) (string, error) {
		masked, err := privacyContext.session.Redact(ctx, value)
		if err != nil {
			return "", err
		}
		if privacyContext.policy.Response == config.PIIResponseRestore {
			masked = privacyContext.session.Restore(masked)
		}
		return masked, nil
	}
	if err := walkFileMetadata(&document, transform); err != nil {
		return nil, err
	}
	return json.Marshal(document)
}

func walkFileMetadata(value *any, transform func(string) (string, error)) error {
	switch typed := (*value).(type) {
	case []any:
		for index := range typed {
			if err := walkFileMetadata(&typed[index], transform); err != nil {
				return err
			}
		}
	case map[string]any:
		for _, field := range []string{"filename", "purpose"} {
			if raw, ok := typed[field].(string); ok {
				transformed, err := transform(raw)
				if err != nil {
					return err
				}
				typed[field] = transformed
			}
		}
		for _, field := range []string{"data"} {
			if nested, exists := typed[field]; exists {
				if err := walkFileMetadata(&nested, transform); err != nil {
					return err
				}
				typed[field] = nested
			}
		}
	}
	return nil
}

func transformFileRecords(
	ctx context.Context,
	records []responsesstate.FileRecord,
	privacyContext filePIIContext,
) error {
	for index := range records {
		for field, value := range map[string]string{
			"filename": records[index].Filename,
			"purpose":  records[index].Purpose,
		} {
			masked, err := privacyContext.session.Redact(ctx, value)
			if err != nil {
				return err
			}
			if privacyContext.policy.Response == config.PIIResponseRestore {
				masked = privacyContext.session.Restore(masked)
			}
			if field == "filename" {
				records[index].Filename = masked
			} else {
				records[index].Purpose = masked
			}
		}
	}
	return nil
}

func transformFileTextResponse(
	ctx context.Context,
	payload []byte,
	privacyContext filePIIContext,
) ([]byte, error) {
	if !utf8.Valid(payload) {
		return nil, fmt.Errorf("text file response is not valid UTF-8")
	}
	masked, err := privacyContext.session.Redact(ctx, string(payload))
	if err != nil {
		return nil, err
	}
	if privacyContext.policy.Response == config.PIIResponseRestore {
		masked = privacyContext.session.Restore(masked)
	}
	return []byte(masked), nil
}
