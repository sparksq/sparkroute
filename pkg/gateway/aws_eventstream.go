// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"strings"
)

const (
	awsEventPreludeBytes    = 12
	awsEventOverheadBytes   = 16
	maxAWSEventPayloadBytes = 16 << 20
	maxAWSEventHeaderBytes  = 128 << 10
	maxAWSEventMessageBytes = maxAWSEventPayloadBytes +
		maxAWSEventHeaderBytes + awsEventOverheadBytes
)

type awsEventMessage struct {
	raw     []byte
	headers map[string]string
	payload []byte
}

func (m awsEventMessage) header(name string) string {
	return m.headers[strings.ToLower(name)]
}

func readAWSEventMessage(source io.Reader) (awsEventMessage, error) {
	var prelude [awsEventPreludeBytes]byte
	read, err := io.ReadFull(source, prelude[:])
	if err != nil {
		if err == io.EOF && read == 0 {
			return awsEventMessage{}, io.EOF
		}
		return awsEventMessage{}, io.ErrUnexpectedEOF
	}
	totalLength := int(binary.BigEndian.Uint32(prelude[0:4]))
	headersLength := int(binary.BigEndian.Uint32(prelude[4:8]))
	expectedPreludeCRC := binary.BigEndian.Uint32(prelude[8:12])
	if totalLength < awsEventOverheadBytes ||
		totalLength > maxAWSEventMessageBytes {
		return awsEventMessage{}, fmt.Errorf(
			"AWS event-stream message length is invalid",
		)
	}
	if headersLength > maxAWSEventHeaderBytes ||
		headersLength > totalLength-awsEventOverheadBytes {
		return awsEventMessage{}, fmt.Errorf(
			"AWS event-stream header length is invalid",
		)
	}
	if totalLength-awsEventOverheadBytes-headersLength >
		maxAWSEventPayloadBytes {
		return awsEventMessage{}, fmt.Errorf(
			"AWS event-stream payload length is invalid",
		)
	}
	if crc32.ChecksumIEEE(prelude[:8]) != expectedPreludeCRC {
		return awsEventMessage{}, fmt.Errorf(
			"AWS event-stream prelude checksum is invalid",
		)
	}
	raw := make([]byte, totalLength)
	copy(raw, prelude[:])
	if _, err := io.ReadFull(
		source,
		raw[awsEventPreludeBytes:],
	); err != nil {
		return awsEventMessage{}, io.ErrUnexpectedEOF
	}
	expectedMessageCRC := binary.BigEndian.Uint32(
		raw[totalLength-4:],
	)
	if crc32.ChecksumIEEE(raw[:totalLength-4]) != expectedMessageCRC {
		return awsEventMessage{}, fmt.Errorf(
			"AWS event-stream message checksum is invalid",
		)
	}
	headers, err := decodeAWSEventHeaders(
		raw[awsEventPreludeBytes : awsEventPreludeBytes+headersLength],
	)
	if err != nil {
		return awsEventMessage{}, err
	}
	payload := raw[awsEventPreludeBytes+headersLength : totalLength-4]
	return awsEventMessage{
		raw:     raw,
		headers: headers,
		payload: payload,
	}, nil
}

func decodeAWSEventHeaders(raw []byte) (map[string]string, error) {
	headers := make(map[string]string)
	for len(raw) != 0 {
		nameLength := int(raw[0])
		raw = raw[1:]
		if nameLength == 0 || len(raw) < nameLength+1 {
			return nil, fmt.Errorf(
				"AWS event-stream header is truncated",
			)
		}
		name := strings.ToLower(string(raw[:nameLength]))
		raw = raw[nameLength:]
		valueType := raw[0]
		raw = raw[1:]
		value, consumed, err := decodeAWSEventHeaderValue(
			valueType,
			raw,
		)
		if err != nil {
			return nil, err
		}
		raw = raw[consumed:]
		if _, exists := headers[name]; exists {
			return nil, fmt.Errorf(
				"AWS event-stream contains a duplicate header",
			)
		}
		headers[name] = value
	}
	return headers, nil
}

func decodeAWSEventHeaderValue(
	valueType byte,
	raw []byte,
) (string, int, error) {
	switch valueType {
	case 0:
		return "true", 0, nil
	case 1:
		return "false", 0, nil
	case 2:
		if len(raw) < 1 {
			break
		}
		return "", 1, nil
	case 3:
		if len(raw) < 2 {
			break
		}
		return "", 2, nil
	case 4:
		if len(raw) < 4 {
			break
		}
		return "", 4, nil
	case 5, 8:
		if len(raw) < 8 {
			break
		}
		return "", 8, nil
	case 6, 7:
		if len(raw) < 2 {
			break
		}
		length := int(binary.BigEndian.Uint16(raw[:2]))
		if len(raw) < 2+length {
			break
		}
		if valueType == 7 {
			return string(raw[2 : 2+length]), 2 + length, nil
		}
		return "", 2 + length, nil
	case 9:
		if len(raw) < 16 {
			break
		}
		return "", 16, nil
	default:
		return "", 0, fmt.Errorf(
			"AWS event-stream header has an unknown value type",
		)
	}
	return "", 0, fmt.Errorf(
		"AWS event-stream header value is truncated",
	)
}

func proxyAWSEventMessages(
	destination io.Writer,
	source io.Reader,
	observe func(awsEventMessage) error,
) error {
	for {
		message, err := readAWSEventMessage(source)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if observe != nil {
			if err := observe(message); err != nil {
				return err
			}
		}
		if _, err := destination.Write(message.raw); err != nil {
			return err
		}
		if flusher, ok := destination.(interface{ Flush() }); ok {
			flusher.Flush()
		}
	}
}

func replaceAWSEventPayload(message awsEventMessage, payload []byte) ([]byte, error) {
	if len(payload) > maxAWSEventPayloadBytes {
		return nil, fmt.Errorf("AWS event-stream payload exceeds %d bytes", maxAWSEventPayloadBytes)
	}
	headersLength := int(binary.BigEndian.Uint32(message.raw[4:8]))
	totalLength := awsEventOverheadBytes + headersLength + len(payload)
	if totalLength > maxAWSEventMessageBytes {
		return nil, fmt.Errorf("AWS event-stream message exceeds %d bytes", maxAWSEventMessageBytes)
	}
	result := make([]byte, totalLength)
	binary.BigEndian.PutUint32(result[0:4], uint32(totalLength))
	binary.BigEndian.PutUint32(result[4:8], uint32(headersLength))
	binary.BigEndian.PutUint32(result[8:12], crc32.ChecksumIEEE(result[:8]))
	copy(
		result[awsEventPreludeBytes:awsEventPreludeBytes+headersLength],
		message.raw[awsEventPreludeBytes:awsEventPreludeBytes+headersLength],
	)
	copy(result[awsEventPreludeBytes+headersLength:totalLength-4], payload)
	binary.BigEndian.PutUint32(result[totalLength-4:], crc32.ChecksumIEEE(result[:totalLength-4]))
	return result, nil
}
