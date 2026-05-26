package iam

import (
	"net"
	"strconv"
	"strings"
	"time"
)

// ConditionContext holds request-scoped values for condition evaluation.
type ConditionContext map[string]string

// ConditionBlock maps operator → {key: expected_value}.
// All conditions must match (AND logic).
type ConditionBlock map[string]map[string]string

// EvaluateConditions evaluates a condition block against a context.
// Returns true if all conditions are satisfied (AND logic).
// Returns true if the block is nil or empty.
func EvaluateConditions(block ConditionBlock, ctx ConditionContext) bool {
	if len(block) == 0 {
		return true
	}
	for operator, conditions := range block {
		for key, expected := range conditions {
			actual := ctx[key]
			if !evaluateOperator(operator, actual, expected) {
				return false
			}
		}
	}
	return true
}

func evaluateOperator(operator, actual, expected string) bool {
	switch operator {
	case "StringEquals":
		return actual == expected
	case "StringNotEquals":
		return actual != expected
	case "StringLike":
		if actual == "" {
			return false
		}
		return globMatch(expected, actual)
	case "IpAddress":
		return actual != "" && matchCIDR(actual, expected)
	case "NotIpAddress":
		return actual == "" || !matchCIDR(actual, expected)
	case "NumericLessThan":
		a, errA := strconv.ParseFloat(actual, 64)
		e, errE := strconv.ParseFloat(expected, 64)
		return errA == nil && errE == nil && a < e
	case "NumericGreaterThan":
		a, errA := strconv.ParseFloat(actual, 64)
		e, errE := strconv.ParseFloat(expected, 64)
		return errA == nil && errE == nil && a > e
	case "NumericEquals":
		a, errA := strconv.ParseFloat(actual, 64)
		e, errE := strconv.ParseFloat(expected, 64)
		return errA == nil && errE == nil && a == e
	case "DateLessThan":
		a, errA := time.Parse(time.RFC3339, actual)
		e, errE := time.Parse(time.RFC3339, expected)
		return errA == nil && errE == nil && a.Before(e)
	case "DateGreaterThan":
		a, errA := time.Parse(time.RFC3339, actual)
		e, errE := time.Parse(time.RFC3339, expected)
		return errA == nil && errE == nil && a.After(e)
	default:
		// Unknown operator — fail closed
		return false
	}
}

// matchCIDR checks if an IPv4 address matches a CIDR range.
func matchCIDR(ip, cidr string) bool {
	if !strings.Contains(cidr, "/") {
		return ip == cidr
	}
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return false
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	return ipNet.Contains(parsed)
}
