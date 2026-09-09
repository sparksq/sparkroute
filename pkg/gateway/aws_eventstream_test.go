// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"io"
	"sort"
	"strings"
	"testing"
)

func TestAWSEventStreamRoundTripAcrossOneByteReads(t *testing.T) {
	t.Parallel()

	raw := encodeAWSEventMessage(t, map[string]string{
		":message-type": "event",
		":event-type":   "contentBlockDelta",
		":content-type": "application/json",
	}, []byte(`{"contentBlockDelta":{"delta":{"text":"hello"}}}`))
	message, err := readAWSEventMessage(
		oneByteReader{source: strings.NewReader(string(raw))},
	)
	if err != nil {
		t.Fatalf("readAWSEventMessage() error = %v", err)
	}
	if !bytes.Equal(message.raw, raw) ||
		message.header(":event-type") != "contentBlockDelta" ||
		string(message.payload) !=
			`{"contentBlockDelta":{"delta":{"text":"hello"}}}` {
		t.Fatalf("message = %#v", message)
	}
	if _, err := readAWSEventMessage(
		bytes.NewReader(nil),
	); err != io.EOF {
		t.Fatalf("empty error = %v, want EOF", err)
	}
}

func TestAWSEventStreamRejectsCorruptChecksum(t *testing.T) {
	t.Parallel()

	raw := encodeAWSEventMessage(t, map[string]string{
		":message-type": "event",
		":event-type":   "messageStop",
	}, []byte(`{"messageStop":{"stopReason":"end_turn"}}`))
	raw[len(raw)-1] ^= 0xff
	if _, err := readAWSEventMessage(
		bytes.NewReader(raw),
	); err == nil {
		t.Fatal("readAWSEventMessage() error = nil")
	}
}

func encodeAWSEventMessage(
	t *testing.T,
	headers map[string]string,
	payload []byte,
) []byte {
	t.Helper()
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	var encodedHeaders bytes.Buffer
	for _, name := range names {
		value := headers[name]
		if len(name) > 255 || len(value) > 65535 {
			t.Fatal("test AWS event header is too long")
		}
		encodedHeaders.WriteByte(byte(len(name)))
		encodedHeaders.WriteString(name)
		encodedHeaders.WriteByte(7)
		var length [2]byte
		binary.BigEndian.PutUint16(length[:], uint16(len(value)))
		encodedHeaders.Write(length[:])
		encodedHeaders.WriteString(value)
	}
	total := awsEventOverheadBytes +
		encodedHeaders.Len() + len(payload)
	raw := make([]byte, total)
	binary.BigEndian.PutUint32(raw[0:4], uint32(total))
	binary.BigEndian.PutUint32(
		raw[4:8],
		uint32(encodedHeaders.Len()),
	)
	binary.BigEndian.PutUint32(
		raw[8:12],
		crc32.ChecksumIEEE(raw[:8]),
	)
	offset := awsEventPreludeBytes
	copy(raw[offset:], encodedHeaders.Bytes())
	offset += encodedHeaders.Len()
	copy(raw[offset:], payload)
	binary.BigEndian.PutUint32(
		raw[total-4:],
		crc32.ChecksumIEEE(raw[:total-4]),
	)
	return raw
}
