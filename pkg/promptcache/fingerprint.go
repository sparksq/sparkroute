// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package promptcache

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
)

type FingerprintOptions struct {
	ScopeDigest    string
	VirtualModel   string
	Operation      string
	ConfigRevision string
	SequenceField  string
	MinPrefixBytes int
	MaxPrefixes    int
}

type Fingerprinter struct {
	key []byte
}

func NewFingerprinter(key []byte) (*Fingerprinter, error) {
	if len(key) < 32 {
		return nil, fmt.Errorf("prompt-cache fingerprint key must be at least 32 bytes")
	}
	return &Fingerprinter{key: append([]byte(nil), key...)}, nil
}

func NewRandomFingerprinter() (*Fingerprinter, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate prompt-cache fingerprint key: %w", err)
	}
	return NewFingerprinter(key)
}

func (f *Fingerprinter) ScopeDigest(scope string) string {
	return f.digest([]byte("scope-v1\x00" + scope))
}

func (f *Fingerprinter) Prefixes(
	envelope map[string]json.RawMessage,
	options FingerprintOptions,
) ([]Prefix, error) {
	if f == nil || options.SequenceField == "" || options.MaxPrefixes < 1 {
		return nil, nil
	}
	sequenceRaw, exists := envelope[options.SequenceField]
	if !exists {
		return nil, nil
	}
	items, err := sequenceItems(sequenceRaw)
	if err != nil {
		return nil, err
	}
	static := make(map[string]json.RawMessage, len(envelope))
	for name, value := range envelope {
		if name == options.SequenceField || ignoredOutputField(name) {
			continue
		}
		static[name] = value
	}
	staticBytes, err := json.Marshal(static)
	if err != nil {
		return nil, fmt.Errorf("marshal prompt-cache static envelope: %w", err)
	}
	seed := []byte("prefix-v1\x00" + options.ScopeDigest + "\x00" +
		options.ConfigRevision + "\x00" + options.VirtualModel + "\x00" +
		options.Operation + "\x00" + options.SequenceField + "\x00")
	seed = append(seed, staticBytes...)
	previous := f.digestBytes(seed)
	prefixBytes := len(staticBytes)
	all := make([]Prefix, 0, len(items))
	for index, item := range items {
		canonical, canonicalErr := canonicalJSON(item)
		if canonicalErr != nil {
			return nil, canonicalErr
		}
		length := make([]byte, 8)
		binary.BigEndian.PutUint64(length, uint64(len(canonical)))
		material := make([]byte, 0, len(previous)+len(length)+len(canonical))
		material = append(material, previous...)
		material = append(material, length...)
		material = append(material, canonical...)
		previous = f.digestBytes(material)
		prefixBytes += len(canonical)
		if prefixBytes >= options.MinPrefixBytes {
			all = append(all, Prefix{
				Digest: encodeDigest(previous), Bytes: prefixBytes, Segments: index + 1,
			})
		}
	}
	if len(all) > options.MaxPrefixes {
		all = all[len(all)-options.MaxPrefixes:]
	}
	for left, right := 0, len(all)-1; left < right; left, right = left+1, right-1 {
		all[left], all[right] = all[right], all[left]
	}
	return all, nil
}

func sequenceItems(raw json.RawMessage) ([]json.RawMessage, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err == nil {
		return items, nil
	}
	var single any
	if err := json.Unmarshal(raw, &single); err != nil {
		return nil, fmt.Errorf("decode prompt-cache sequence: %w", err)
	}
	return []json.RawMessage{append(json.RawMessage(nil), raw...)}, nil
}

func canonicalJSON(raw json.RawMessage) ([]byte, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("decode prompt-cache sequence item: %w", err)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal prompt-cache sequence item: %w", err)
	}
	return canonical, nil
}

func ignoredOutputField(name string) bool {
	switch name {
	case "model", "stream", "stream_options", "max_tokens", "max_completion_tokens",
		"max_output_tokens", "temperature", "top_p", "top_k", "seed", "n",
		"logprobs", "top_logprobs", "service_tier", "store", "metadata",
		"user", "background":
		return true
	default:
		return false
	}
}

func (f *Fingerprinter) digest(material []byte) string {
	return encodeDigest(f.digestBytes(material))
}

func (f *Fingerprinter) digestBytes(material []byte) []byte {
	mac := hmac.New(sha256.New, f.key)
	_, _ = mac.Write(material)
	return mac.Sum(nil)
}

func encodeDigest(value []byte) string {
	return base64.RawURLEncoding.EncodeToString(value)
}
