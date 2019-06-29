package tagquery

import (
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
)

type expressionMatchTag struct {
	expressionCommon
	valueRe *regexp.Regexp
}

func (e *expressionMatchTag) GetOperator() ExpressionOperator {
	return MATCH_TAG
}

func (e *expressionMatchTag) HasRe() bool {
	return true
}

func (e *expressionMatchTag) ValuePasses(tag string) bool {
	return e.valueRe.MatchString(tag)
}

func (e *expressionMatchTag) GetDefaultDecision() FilterDecision {
	return Fail
}

func (e *expressionMatchTag) OperatesOnTag() bool {
	return true
}

func (e *expressionMatchTag) StringIntoBuilder(builder *strings.Builder) {
	builder.WriteString("__tag=~")
	builder.WriteString(e.value)
}

func (e *expressionMatchTag) GetMetricDefinitionFilter() MetricDefinitionFilter {
	if e.valueRe.Match([]byte("name")) {
		// every metric has a tag name, so we can always return Pass
		return func(_ string, _ []string) FilterDecision { return Pass }
	}

	var matchCache, missCache sync.Map
	var currentMatchCacheSize, currentMissCacheSize int32

	return func(_ string, tags []string) FilterDecision {
		for _, tag := range tags {
			values := strings.SplitN(tag, "=", 2)
			if len(values) < 2 {
				continue
			}
			value := values[0]

			if _, ok := missCache.Load(value); ok {
				continue
			}

			if _, ok := matchCache.Load(value); ok {
				return Pass
			}

			if e.valueRe.Match([]byte(value)) {
				if atomic.LoadInt32(&currentMatchCacheSize) < int32(matchCacheSize) {
					matchCache.Store(value, struct{}{})
					atomic.AddInt32(&currentMatchCacheSize, 1)
				}
				return Pass
			} else {
				if atomic.LoadInt32(&currentMissCacheSize) < int32(matchCacheSize) {
					missCache.Store(value, struct{}{})
					atomic.AddInt32(&currentMissCacheSize, 1)
				}
				continue
			}
		}

		return None
	}
}