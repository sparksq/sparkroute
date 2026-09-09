// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/routing"
)

const maxSSEEventBytes = 1 << 20

func presentedModelName(selection routing.Selection) (string, bool) {
	switch selection.ResponseModel {
	case config.ResponseModelUpstream:
		return "", false
	case config.ResponseModelRequested:
		return selection.RequestedModel, true
	case config.ResponseModelVirtual:
		return selection.VirtualModel, true
	default:
		return selection.VirtualModel, true
	}
}

func readBoundedResponse(source io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(source, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	return body, nil
}

func rewriteJSONResponseModel(body []byte, model string) ([]byte, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil || envelope == nil {
		if err == nil {
			err = fmt.Errorf("response must be a JSON object")
		}
		return nil, err
	}
	if _, exists := envelope["model"]; !exists {
		return body, nil
	}
	rawModel, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	envelope["model"] = rawModel
	return json.Marshal(envelope)
}

func validateJSONResponse(body []byte) error {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return err
	}
	if envelope == nil {
		return fmt.Errorf("response must be a JSON object")
	}
	return nil
}

// rewriteSSEModels buffers at most one SSE event, never the stream. It parses
// complete events across arbitrary transport chunk boundaries, rewrites only a
// top-level JSON model field, and flushes after each event.
func rewriteSSEModels(destination io.Writer, source io.Reader, model string) error {
	return proxySSEEvents(destination, source, func(event []byte) []byte {
		return rewriteSSEEventModel(event, model)
	}, nil)
}

func proxySSEEvents(
	destination io.Writer,
	source io.Reader,
	transformEvent func([]byte) []byte,
	observePayload func([]byte) error,
) error {
	event := make([]byte, 0, 4096)
	scanIndex := 0
	lineStart := 0
	buffer := make([]byte, 32<<10)
	for {
		read, err := source.Read(buffer)
		if read > 0 {
			event = append(event, buffer[:read]...)
			var processErr error
			event, scanIndex, lineStart, processErr = writeCompleteSSEEvents(
				destination,
				event,
				scanIndex,
				lineStart,
				false,
				transformEvent,
				observePayload,
			)
			if processErr != nil {
				return processErr
			}
			if len(event) > maxSSEEventBytes {
				return fmt.Errorf("SSE event exceeds %d bytes", maxSSEEventBytes)
			}
		}
		if err != nil && err != io.EOF {
			return err
		}
		if err == io.EOF {
			var processErr error
			event, _, _, processErr = writeCompleteSSEEvents(
				destination,
				event,
				scanIndex,
				lineStart,
				true,
				transformEvent,
				observePayload,
			)
			if processErr != nil {
				return processErr
			}
			if len(event) > 0 {
				if err := writeSSEEvent(
					destination,
					event,
					transformEvent,
					observePayload,
				); err != nil {
					return err
				}
			}
			return nil
		}
		if read == 0 {
			return io.ErrNoProgress
		}
	}
}

func writeCompleteSSEEvents(
	destination io.Writer,
	event []byte,
	scanIndex int,
	lineStart int,
	eof bool,
	transformEvent func([]byte) []byte,
	observePayload func([]byte) error,
) ([]byte, int, int, error) {
	for scanIndex < len(event) {
		if event[scanIndex] != '\r' && event[scanIndex] != '\n' {
			scanIndex++
			continue
		}
		endingBytes := 1
		if event[scanIndex] == '\r' {
			if scanIndex+1 == len(event) && !eof {
				break
			}
			if scanIndex+1 < len(event) && event[scanIndex+1] == '\n' {
				endingBytes = 2
			}
		}
		lineIsBlank := scanIndex == lineStart
		scanIndex += endingBytes
		lineStart = scanIndex
		if !lineIsBlank {
			continue
		}
		if scanIndex > maxSSEEventBytes {
			return nil, 0, 0, fmt.Errorf(
				"SSE event exceeds %d bytes",
				maxSSEEventBytes,
			)
		}
		if err := writeSSEEvent(
			destination,
			event[:scanIndex],
			transformEvent,
			observePayload,
		); err != nil {
			return nil, 0, 0, err
		}
		event = event[scanIndex:]
		lineStart = 0
		scanIndex = 0
	}
	return event, scanIndex, lineStart, nil
}

func writeSSEEvent(
	destination io.Writer,
	event []byte,
	transformEvent func([]byte) []byte,
	observePayload func([]byte) error,
) error {
	if observePayload != nil {
		if payload, exists := sseDataPayload(event); exists {
			if err := observePayload(payload); err != nil {
				return err
			}
		}
	}
	output := event
	if transformEvent != nil {
		output = transformEvent(event)
	}
	if _, err := destination.Write(output); err != nil {
		return err
	}
	if flusher, ok := destination.(interface{ Flush() }); ok {
		flusher.Flush()
	}
	return nil
}

func sseDataPayload(event []byte) ([]byte, bool) {
	lines := splitSSELines(event)
	data := make([][]byte, 0, 1)
	for index := range lines {
		content := lines[index].content
		switch {
		case bytes.Equal(content, []byte("data")):
			data = append(data, nil)
		case bytes.HasPrefix(content, []byte("data:")):
			value := content[len("data:"):]
			data = append(data, bytes.TrimPrefix(value, []byte{' '}))
		}
	}
	if len(data) == 0 {
		return nil, false
	}
	return bytes.Join(data, []byte{'\n'}), true
}

type sseLine struct {
	raw     []byte
	content []byte
	ending  []byte
	isData  bool
	data    []byte
}

func rewriteSSEEventModel(event []byte, model string) []byte {
	rawModel, err := json.Marshal(model)
	if err != nil {
		return event
	}
	return rewriteSSEEventJSON(event, len(model), func(envelope map[string]json.RawMessage) bool {
		if _, exists := envelope["model"]; !exists {
			return false
		}
		envelope["model"] = rawModel
		return true
	})
}

func rewriteResponsesSSEEventModel(event []byte, model string) []byte {
	rawModel, err := json.Marshal(model)
	if err != nil {
		return event
	}
	return rewriteSSEEventJSON(event, len(model), func(envelope map[string]json.RawMessage) bool {
		rawResponse, exists := envelope["response"]
		if !exists {
			return false
		}
		var response map[string]json.RawMessage
		if err := json.Unmarshal(rawResponse, &response); err != nil || response == nil {
			return false
		}
		if _, exists := response["model"]; !exists {
			return false
		}
		response["model"] = rawModel
		encoded, err := json.Marshal(response)
		if err != nil {
			return false
		}
		envelope["response"] = encoded
		return true
	})
}

func rewriteSSEEventJSON(
	event []byte,
	growth int,
	mutate func(map[string]json.RawMessage) bool,
) []byte {
	lines := splitSSELines(event)
	data := make([][]byte, 0, 1)
	firstData := -1
	for index := range lines {
		content := lines[index].content
		switch {
		case bytes.Equal(content, []byte("data")):
			lines[index].isData = true
			lines[index].data = nil
		case bytes.HasPrefix(content, []byte("data:")):
			lines[index].isData = true
			value := content[len("data:"):]
			value = bytes.TrimPrefix(value, []byte{' '})
			lines[index].data = value
		default:
			continue
		}
		if firstData < 0 {
			firstData = index
		}
		data = append(data, lines[index].data)
	}
	if firstData < 0 {
		return event
	}
	payload := bytes.Join(data, []byte{'\n'})
	if bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
		return event
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return event
	}
	if !mutate(envelope) {
		return event
	}
	rewritten, err := json.Marshal(envelope)
	if err != nil {
		return event
	}
	payload = rewritten

	var result bytes.Buffer
	result.Grow(len(event) + growth)
	for index, line := range lines {
		if !line.isData {
			result.Write(line.raw)
			continue
		}
		if index == firstData {
			result.WriteString("data: ")
			result.Write(payload)
			result.Write(line.ending)
		}
	}
	return result.Bytes()
}

func splitSSELines(event []byte) []sseLine {
	lines := make([]sseLine, 0, bytes.Count(event, []byte{'\n'})+1)
	for len(event) > 0 {
		lineEnd := -1
		for index, value := range event {
			if value == '\r' || value == '\n' {
				lineEnd = index
				break
			}
		}
		if lineEnd < 0 {
			lines = append(lines, sseLine{
				raw:     event,
				content: event,
			})
			break
		}
		endingBytes := 1
		if event[lineEnd] == '\r' &&
			lineEnd+1 < len(event) &&
			event[lineEnd+1] == '\n' {
			endingBytes = 2
		}
		raw := event[:lineEnd+endingBytes]
		content := event[:lineEnd]
		ending := event[lineEnd : lineEnd+endingBytes]
		lines = append(lines, sseLine{
			raw:     raw,
			content: content,
			ending:  ending,
		})
		event = event[lineEnd+endingBytes:]
	}
	return lines
}
