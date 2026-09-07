package gateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	credentialawsworkload "github.com/sparksq/sparkroute/pkg/credentials/awsworkload"
)

const (
	awsSigV4Algorithm = "AWS4-HMAC-SHA256"
	bedrockAWSService = "bedrock"
)

func signAWSRequest(
	request *http.Request,
	rawMaterial []byte,
	region string,
	service string,
	body []byte,
	signingTime time.Time,
) error {
	material, err := credentialawsworkload.DecodeSigningMaterial(
		rawMaterial,
	)
	if err != nil {
		return err
	}
	signingTime = signingTime.UTC()
	if !material.ExpiresAt.IsZero() &&
		!signingTime.Before(material.ExpiresAt) {
		return fmt.Errorf("AWS signing credentials are expired")
	}
	if strings.ContainsAny(
		material.AccessKeyID+
			material.SecretAccessKey+
			material.SessionToken,
		"\r\n",
	) {
		return fmt.Errorf("AWS signing credentials contain a newline")
	}
	if region == "" || service == "" {
		return fmt.Errorf("AWS signing region and service are required")
	}
	payloadDigest := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(payloadDigest[:])
	amzDate := signingTime.Format("20060102T150405Z")
	shortDate := signingTime.Format("20060102")

	request.Header.Del("Authorization")
	request.Header.Set("X-Amz-Date", amzDate)
	request.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if material.SessionToken == "" {
		request.Header.Del("X-Amz-Security-Token")
	} else {
		request.Header.Set(
			"X-Amz-Security-Token",
			material.SessionToken,
		)
	}
	if request.Header.Get("Content-Type") == "" {
		request.Header.Set("Content-Type", "application/json")
	}

	canonicalURI, err := canonicalAWSURI(request.URL)
	if err != nil {
		return err
	}
	canonicalQuery, err := canonicalAWSQuery(request.URL)
	if err != nil {
		return err
	}
	host := request.URL.Host
	if request.Host != "" {
		host = request.Host
	}
	headers := map[string]string{
		"content-type": canonicalAWSHeaderValue(
			request.Header.Get("Content-Type"),
		),
		"host":                 canonicalAWSHeaderValue(host),
		"x-amz-content-sha256": payloadHash,
		"x-amz-date":           amzDate,
	}
	if material.SessionToken != "" {
		headers["x-amz-security-token"] = canonicalAWSHeaderValue(
			material.SessionToken,
		)
	}
	headerNames := make([]string, 0, len(headers))
	for name := range headers {
		headerNames = append(headerNames, name)
	}
	sort.Strings(headerNames)
	var canonicalHeaders strings.Builder
	for _, name := range headerNames {
		canonicalHeaders.WriteString(name)
		canonicalHeaders.WriteByte(':')
		canonicalHeaders.WriteString(headers[name])
		canonicalHeaders.WriteByte('\n')
	}
	signedHeaders := strings.Join(headerNames, ";")
	canonicalRequest := strings.Join([]string{
		request.Method,
		canonicalURI,
		canonicalQuery,
		canonicalHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")
	canonicalDigest := sha256.Sum256([]byte(canonicalRequest))
	scope := strings.Join(
		[]string{shortDate, region, service, "aws4_request"},
		"/",
	)
	stringToSign := strings.Join([]string{
		awsSigV4Algorithm,
		amzDate,
		scope,
		hex.EncodeToString(canonicalDigest[:]),
	}, "\n")
	dateKey := awsHMAC(
		[]byte("AWS4"+material.SecretAccessKey),
		shortDate,
	)
	regionKey := awsHMAC(dateKey, region)
	serviceKey := awsHMAC(regionKey, service)
	signingKey := awsHMAC(serviceKey, "aws4_request")
	signature := hex.EncodeToString(
		awsHMAC(signingKey, stringToSign),
	)
	request.Header.Set(
		"Authorization",
		awsSigV4Algorithm+" Credential="+
			material.AccessKeyID+"/"+scope+
			", SignedHeaders="+signedHeaders+
			", Signature="+signature,
	)
	return nil
}

func awsHMAC(key []byte, value string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(value))
	return mac.Sum(nil)
}

func canonicalAWSURI(value *url.URL) (string, error) {
	escaped := value.EscapedPath()
	if escaped == "" {
		return "/", nil
	}
	// The standard AWS signer applies Amazon URI encoding to the already
	// escaped wire path. This intentionally double-encodes '%' from URI-label
	// escapes such as an ARN's ':' or embedded '/'.
	return awsURIEncode(escaped, true), nil
}

func canonicalAWSQuery(value *url.URL) (string, error) {
	if value.RawQuery == "" {
		return "", nil
	}
	values, err := url.ParseQuery(value.RawQuery)
	if err != nil {
		return "", fmt.Errorf("decode AWS request query: %w", err)
	}
	type pair struct {
		key   string
		value string
	}
	pairs := make([]pair, 0)
	for key, items := range values {
		if len(items) == 0 {
			items = []string{""}
		}
		for _, item := range items {
			pairs = append(pairs, pair{
				key:   awsURIEncode(key, false),
				value: awsURIEncode(item, false),
			})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].key == pairs[j].key {
			return pairs[i].value < pairs[j].value
		}
		return pairs[i].key < pairs[j].key
	})
	var result strings.Builder
	for index, item := range pairs {
		if index != 0 {
			result.WriteByte('&')
		}
		result.WriteString(item.key)
		result.WriteByte('=')
		result.WriteString(item.value)
	}
	return result.String(), nil
}

func awsURIEncode(value string, preserveSlash bool) string {
	const upperHex = "0123456789ABCDEF"
	var encoded strings.Builder
	encoded.Grow(len(value))
	for index := 0; index < len(value); index++ {
		char := value[index]
		if char >= 'a' && char <= 'z' ||
			char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' ||
			char == '-' || char == '.' ||
			char == '_' || char == '~' ||
			char == '/' && preserveSlash {
			encoded.WriteByte(char)
			continue
		}
		encoded.WriteByte('%')
		encoded.WriteByte(upperHex[char>>4])
		encoded.WriteByte(upperHex[char&0x0f])
	}
	return encoded.String()
}

func canonicalAWSHeaderValue(value string) string {
	return strings.Join(strings.Fields(value), " ")
}
