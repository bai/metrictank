package tagquery

import (
	"strings"

	"github.com/raintank/schema"
)

type expressionNotEqual struct {
	expressionCommon
}

func (e *expressionNotEqual) Equals(other Expression) bool {
	return e.key == other.GetKey() && e.GetOperator() == other.GetOperator() && e.value == other.GetValue()
}

func (e *expressionNotEqual) GetDefaultDecision() FilterDecision {
	return Pass
}

func (e *expressionNotEqual) GetOperator() ExpressionOperator {
	return NOT_EQUAL
}

func (e *expressionNotEqual) GetCostMultiplier() uint32 {
	return 1
}

func (e *expressionNotEqual) RequiresNonEmptyValue() bool {
	return false
}

func (e *expressionNotEqual) ValuePasses(value string) bool {
	return value != e.value
}

func (e *expressionNotEqual) ValueMatchesExactly() bool {
	return true
}

func (e *expressionNotEqual) GetMetricDefinitionFilter(lookup IdTagLookup) MetricDefinitionFilter {
	if e.key == "name" {
		if e.value == "" {
			return func(_ schema.MKey, _ string, _ []string) FilterDecision { return Pass }
		}

		return func(_ schema.MKey, name string, _ []string) FilterDecision {
			if schema.SanitizeNameAsTagValue(name) == e.value {
				return Fail
			}
			return Pass
		}
	}

	if !MetaTagSupport {
		return func(id schema.MKey, _ string, _ []string) FilterDecision {
			if lookup(id, e.key, e.value) {
				return Fail
			}
			return Pass
		}
	}

	prefix := e.key + "="
	return func(id schema.MKey, _ string, tags []string) FilterDecision {
		if lookup(id, e.key, e.value) {
			return Fail
		}

		for _, tag := range tags {
			// the tag is set, but it has a different value,
			// no need to keep looking at other indexes
			if strings.HasPrefix(tag, prefix) {
				return Pass
			}
		}

		return None
	}
}

func (e *expressionNotEqual) StringIntoBuilder(builder *strings.Builder) {
	builder.WriteString(e.key)
	builder.WriteString("!=")
	builder.WriteString(e.value)
}
