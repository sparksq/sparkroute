// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package pii

import (
	"context"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type BuiltinDetector struct{}

func NewBuiltinDetector() *BuiltinDetector { return &BuiltinDetector{} }

func (*BuiltinDetector) Supports(entity Entity) bool {
	switch entity {
	case EntityEmail, EntityPhone, EntitySSN, EntityCreditCard, EntityIPv4:
		return true
	default:
		return false
	}
}

var (
	emailPattern      = regexp.MustCompile(`(?i)[a-z0-9.!#$%&'*+/=?^_` + "`" + `{|}~-]+@[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+`)
	phonePattern      = regexp.MustCompile(`[+]?[0-9][0-9() .-]{7,}[0-9]`)
	ssnPattern        = regexp.MustCompile(`[0-9]{3}-[0-9]{2}-[0-9]{4}`)
	creditCardPattern = regexp.MustCompile(`[0-9](?:[0-9 -]{11,21}[0-9])`)
	ipv4Pattern       = regexp.MustCompile(`(?:[0-9]{1,3}\.){3}[0-9]{1,3}`)
)

type detectedSpan struct {
	Span
	priority int
}

func (d *BuiltinDetector) Detect(ctx context.Context, text string, entities []Entity) ([]Span, error) {
	requested := make(map[Entity]struct{}, len(entities))
	for _, entity := range entities {
		requested[entity] = struct{}{}
	}
	var candidates []detectedSpan
	add := func(entity Entity, priority int, indexes [][]int, valid func(string) bool) {
		if _, enabled := requested[entity]; !enabled {
			return
		}
		for _, index := range indexes {
			if ctx.Err() != nil {
				return
			}
			if !asciiBoundary(text, index[0], index[1]) || !valid(text[index[0]:index[1]]) {
				continue
			}
			candidates = append(candidates, detectedSpan{
				Span:     Span{Start: index[0], End: index[1], Entity: entity},
				priority: priority,
			})
		}
	}
	add(EntityEmail, 0, emailPattern.FindAllStringIndex(text, -1), alwaysValid)
	add(EntitySSN, 1, ssnPattern.FindAllStringIndex(text, -1), validSSN)
	add(EntityCreditCard, 2, creditCardPattern.FindAllStringIndex(text, -1), validCreditCard)
	add(EntityIPv4, 3, ipv4Pattern.FindAllStringIndex(text, -1), validIPv4)
	add(EntityPhone, 4, phonePattern.FindAllStringIndex(text, -1), validPhone)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sort.Slice(candidates, func(left, right int) bool {
		if candidates[left].Start != candidates[right].Start {
			return candidates[left].Start < candidates[right].Start
		}
		if candidates[left].priority != candidates[right].priority {
			return candidates[left].priority < candidates[right].priority
		}
		return candidates[left].End > candidates[right].End
	})
	result := make([]Span, 0, len(candidates))
	for _, candidate := range candidates {
		if len(result) != 0 && candidate.Start < result[len(result)-1].End {
			continue
		}
		result = append(result, candidate.Span)
	}
	return result, nil
}

func alwaysValid(string) bool { return true }

func asciiBoundary(text string, start, end int) bool {
	isWord := func(value byte) bool {
		return value >= '0' && value <= '9' || value >= 'A' && value <= 'Z' ||
			value >= 'a' && value <= 'z' || value == '_'
	}
	return (start == 0 || !isWord(text[start-1])) &&
		(end == len(text) || !isWord(text[end]))
}

func digits(value string) string {
	var result strings.Builder
	for _, character := range value {
		if character >= '0' && character <= '9' {
			result.WriteRune(character)
		}
	}
	return result.String()
}

func validPhone(value string) bool {
	count := len(digits(value))
	return count >= 10 && count <= 15
}

func validSSN(value string) bool {
	return len(value) == 11 && value[:3] != "000" && value[4:6] != "00" && value[7:] != "0000"
}

func validCreditCard(value string) bool {
	numbers := digits(value)
	if len(numbers) < 13 || len(numbers) > 19 {
		return false
	}
	sum := 0
	double := false
	for index := len(numbers) - 1; index >= 0; index-- {
		digit := int(numbers[index] - '0')
		if double {
			digit *= 2
			if digit > 9 {
				digit -= 9
			}
		}
		sum += digit
		double = !double
	}
	return sum%10 == 0
}

func validIPv4(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) != 4 {
		return false
	}
	for _, part := range parts {
		if len(part) > 1 && part[0] == '0' {
			return false
		}
		number, err := strconv.Atoi(part)
		if err != nil || number < 0 || number > 255 {
			return false
		}
	}
	return true
}
