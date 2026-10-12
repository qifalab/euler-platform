package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	errorsx "github.com/qifalab/euler-platform/errors"
	"github.com/qifalab/euler-platform/identifier"
)

// eumon: alarm rules and metric data, mapped onto svc-monitor's REST API
// (/api/v1/monitor/rules, /api/v1/monitor/metrics). Rules are scoped to the
// caller's account by svc-monitor; a rule id of another account reads as not
// found.

const (
	maxRulePeriodSeconds = 86400
	maxEvaluationCount   = 1440
	maxChannels          = 10
	// DescribeMetricData Period bounds (svc-monitor metrics.go
	// minMetricPeriod/maxMetricPeriod).
	minMetricPeriodSeconds = 10
	maxMetricPeriodSeconds = 86400
)

// comparisonOperators maps the accepted spellings onto svc-monitor's operator
// numbers (monitor.proto ComparisonOperator: 1 > 2 >= 3 < 4 <=).
var comparisonOperators = map[string]int{
	"GreaterThanThreshold": 1, ">": 1, "1": 1,
	"GreaterThanOrEqualToThreshold": 2, ">=": 2, "2": 2,
	"LessThanThreshold": 3, "<": 3, "3": 3,
	"LessThanOrEqualToThreshold": 4, "<=": 4, "4": 4,
}

var comparisonNames = map[int]string{
	1: "GreaterThanThreshold", 2: "GreaterThanOrEqualToThreshold",
	3: "LessThanThreshold", 4: "LessThanOrEqualToThreshold",
}

var (
	metricNamePattern   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.:-]{0,127}$`)
	resourceTypePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,63}$`)
	channelPattern      = regexp.MustCompile(`^[A-Za-z0-9_.:@-]{1,128}$`)
)

func ruleView(r *alarmRule) map[string]any {
	channels := r.NotificationChannels
	if channels == nil {
		channels = []string{}
	}
	status := "Enabled"
	if r.Status == 2 {
		status = "Disabled"
	}
	return map[string]any{
		"RuleId":               strconv.FormatInt(r.RuleID, 10),
		"ProductCode":          r.ProductCode,
		"ResourceType":         r.ResourceType,
		"MetricName":           r.Metric,
		"Threshold":            r.Threshold,
		"ComparisonOperator":   comparisonNames[r.ComparisonOperator],
		"Period":               r.Period,
		"EvaluationCount":      r.EvalPeriods,
		"NotificationChannels": channels,
		"Status":               status,
		"RuleVersion":          r.Version,
		"CreationTime":         r.CreatedAt,
		"UpdateTime":           r.UpdatedAt,
	}
}

// rule finds one of the caller's rules (svc-monitor's DELETE does not report
// a missing rule, so existence is checked against the caller's own list).
func (s *server) rule(c *call, raw string) (*alarmRule, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return nil, c.notFound()
	}
	rules, err := s.alarmRules(c)
	if err != nil {
		return nil, err
	}
	for i := range rules {
		if rules[i].RuleID == id {
			return &rules[i], nil
		}
	}
	return nil, c.notFound()
}

func (s *server) describeAlarmRules(c *call) (any, error) {
	ids, err := c.p.ids("RuleId", "RuleIds")
	if err != nil {
		return nil, err
	}
	product, err := c.p.str("ProductCode", "Namespace")
	if err != nil {
		return nil, err
	}
	metric, err := c.p.str("MetricName")
	if err != nil {
		return nil, err
	}
	number, size, err := c.page()
	if err != nil {
		return nil, err
	}
	rules, err := s.alarmRules(c)
	if err != nil {
		return nil, err
	}
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].RuleID < rules[j].RuleID })
	items := make([]map[string]any, 0, len(rules))
	for i := range rules {
		r := &rules[i]
		if (len(want) > 0 && !want[strconv.FormatInt(r.RuleID, 10)]) ||
			(product != "" && r.ProductCode != product) || (metric != "" && r.Metric != metric) {
			continue
		}
		items = append(items, ruleView(r))
	}
	return paginate("AlarmRules", items, number, size), nil
}

// ruleFields are the optional rule settings shared by create and modify.
type ruleFields struct {
	threshold  *string
	comparison *int
	period     *int
	evalCount  *int
	channels   *[]string
}

func (c *call) ruleFields() (*ruleFields, error) {
	f := &ruleFields{}
	if c.p.has("Threshold") {
		v, err := c.p.str("Threshold")
		if err != nil {
			return nil, err
		}
		if v == "" {
			return nil, invalidParam("Threshold", "must be a decimal number")
		}
		f.threshold = &v // svc-monitor normalises it (a decimal, at most 4 places)
	}
	if c.p.has("ComparisonOperator") {
		v, err := c.p.str("ComparisonOperator")
		if err != nil {
			return nil, err
		}
		op, ok := comparisonOperators[v]
		if !ok {
			return nil, invalidParam("ComparisonOperator",
				"must be GreaterThanThreshold, GreaterThanOrEqualToThreshold, LessThanThreshold or LessThanOrEqualToThreshold")
		}
		f.comparison = &op
	}
	for _, it := range []struct {
		name string
		max  int64
		dst  **int
	}{{"Period", maxRulePeriodSeconds, &f.period}, {"EvaluationCount", maxEvaluationCount, &f.evalCount}} {
		if !c.p.has(it.name) {
			continue
		}
		n, err := c.p.integer(it.name, 0, 0, it.max)
		if err != nil {
			return nil, err
		}
		v := int(n)
		*it.dst = &v
	}
	if c.p.has("NotificationChannels") {
		chs, err := c.p.list("NotificationChannels")
		if err != nil {
			return nil, err
		}
		if len(chs) > maxChannels {
			return nil, invalidParam("NotificationChannels", "accepts at most 10 channels")
		}
		for _, ch := range chs {
			if !channelPattern.MatchString(ch) {
				return nil, invalidParam("NotificationChannels", "contains a malformed channel")
			}
		}
		f.channels = &chs
	}
	return f, nil
}

// createAlarmRule creates a rule. svc-monitor keys rules on (ProductCode,
// ResourceType, MetricName) per account: creating one that exists updates it
// (an upsert), and the answer carries its RuleId.
func (s *server) createAlarmRule(c *call) (any, error) {
	product, err := c.p.need("ProductCode", "Namespace")
	if err != nil {
		return nil, err
	}
	if !identifier.IsValidProductCode(product) {
		return nil, invalidParam("ProductCode", "must be a product code such as euecs")
	}
	resourceType, err := c.p.str("ResourceType")
	if err != nil {
		return nil, err
	}
	if resourceType != "" && !resourceTypePattern.MatchString(resourceType) {
		return nil, invalidParam("ResourceType", "is not a resource type such as instance")
	}
	metric, err := c.p.need("MetricName")
	if err != nil {
		return nil, err
	}
	if !metricNamePattern.MatchString(metric) {
		return nil, invalidParam("MetricName", "is not a metric name such as cpu_utilization")
	}
	if !c.p.has("Threshold") {
		return nil, missingParam("Threshold")
	}
	f, err := c.ruleFields()
	if err != nil {
		return nil, err
	}
	body := struct {
		ProductCode          string   `json:"productCode"`
		ResourceType         string   `json:"resourceType"`
		Metric               string   `json:"metric"`
		Threshold            string   `json:"threshold"`
		ComparisonOperator   int      `json:"comparisonOperator,omitempty"`
		Period               int      `json:"period,omitempty"`
		EvalPeriods          int      `json:"evalPeriods,omitempty"`
		NotificationChannels []string `json:"notificationChannels,omitempty"`
	}{ProductCode: product, ResourceType: resourceType, Metric: metric, Threshold: *f.threshold}
	if f.comparison != nil {
		body.ComparisonOperator = *f.comparison
	}
	if f.period != nil {
		body.Period = *f.period
	}
	if f.evalCount != nil {
		body.EvalPeriods = *f.evalCount
	}
	if f.channels != nil {
		body.NotificationChannels = *f.channels
	}
	r, err := s.postAlarmRule(c, body)
	if err != nil {
		return nil, err
	}
	return map[string]any{"RuleId": strconv.FormatInt(r.RuleID, 10), "RuleVersion": r.Version}, nil
}

// modifyAlarmRule changes a rule. RuleVersion (optional; "Version" is the API
// version parameter) is the optimistic lock: given, a concurrent change
// answers ConcurrentModification; omitted, the rule's current version is used.
func (s *server) modifyAlarmRule(c *call) (any, error) {
	raw, err := c.p.need("RuleId")
	if err != nil {
		return nil, err
	}
	f, err := c.ruleFields()
	if err != nil {
		return nil, err
	}
	var status *int
	if c.p.has("Status") {
		v, err := c.p.str("Status")
		if err != nil {
			return nil, err
		}
		var st int
		switch strings.ToLower(v) {
		case "enabled", "enable", "1":
			st = 1
		case "disabled", "disable", "2":
			st = 2
		default:
			return nil, invalidParam("Status", "must be Enabled or Disabled")
		}
		status = &st
	}
	if f.threshold == nil && f.comparison == nil && f.period == nil && f.evalCount == nil && f.channels == nil && status == nil {
		return nil, errorsx.New(codeMissingParameter, http.StatusBadRequest,
			"At least one of Threshold, ComparisonOperator, Period, EvaluationCount, NotificationChannels and Status is required.")
	}
	r, err := s.rule(c, raw)
	if err != nil {
		return nil, err
	}
	version, err := c.p.integer("RuleVersion", int64(r.Version), 1, 1<<31-1)
	if err != nil {
		return nil, err
	}
	body := struct {
		Threshold            *string   `json:"threshold,omitempty"`
		ComparisonOperator   *int      `json:"comparisonOperator,omitempty"`
		Period               *int      `json:"period,omitempty"`
		EvalPeriods          *int      `json:"evalPeriods,omitempty"`
		NotificationChannels *[]string `json:"notificationChannels,omitempty"`
		Status               *int      `json:"status,omitempty"`
		Version              int       `json:"version"`
	}{f.threshold, f.comparison, f.period, f.evalCount, f.channels, status, int(version)}
	updated, err := s.updateAlarmRule(c, r.RuleID, body)
	if err != nil {
		return nil, err
	}
	return map[string]any{"RuleId": strconv.FormatInt(updated.RuleID, 10), "RuleVersion": updated.Version}, nil
}

func (s *server) deleteAlarmRuleAction(c *call) (any, error) {
	raw, err := c.p.need("RuleId")
	if err != nil {
		return nil, err
	}
	r, err := s.rule(c, raw)
	if err != nil {
		return nil, err
	}
	if err := s.deleteAlarmRule(c, r.RuleID); err != nil {
		return nil, err
	}
	return map[string]any{"RuleId": strconv.FormatInt(r.RuleID, 10)}, nil
}

// errMetricResourceNotFound answers DescribeMetricData for a resource the
// caller cannot read: another account's, released, or never existing.
var errMetricResourceNotFound = errorsx.New("InvalidResourceId.NotFound", http.StatusNotFound,
	"The specified ResourceId does not exist.")

// metricParamNames renames svc-monitor's query parameters, as its
// Common.InvalidParameter messages name them, to the Action's parameters.
var metricParamNames = regexp.MustCompile(`\b(metric|resourceId|start|end|period)\b`)

var metricParamRename = map[string]string{
	"metric": "MetricName", "resourceId": "ResourceId", "start": "StartTime", "end": "EndTime", "period": "Period",
}

// describeMetricData returns a metric series of one of the caller's resources,
// read from svc-monitor GET /api/v1/monitor/metrics:
//
//	MetricName → metric, ResourceId (alias InstanceId, any product; required)
//	→ resourceId, StartTime/EndTime (RFC 3339, Unix seconds or milliseconds)
//	→ start/end, Period (seconds) → period.
//
// The resource must be the caller's and not released (checked here, like every
// Action, and again by svc-monitor). svc-monitor's verdicts map onto the API
// vocabulary: Monitor.ResourceNotFound → 404 InvalidResourceId.NotFound,
// Common.InvalidParameter (range/period bounds) → 400 InvalidParameter, an
// unavailable metric store or ownership lookup (503) → 503 ServiceUnavailable.
func (s *server) describeMetricData(c *call) (any, error) {
	metric, err := c.p.need("MetricName")
	if err != nil {
		return nil, err
	}
	if !metricNamePattern.MatchString(metric) {
		return nil, invalidParam("MetricName", "is not a metric name such as cpu_utilization")
	}
	resID, err := c.p.need("ResourceId", "InstanceId")
	if err != nil {
		return nil, err
	}
	q := url.Values{"metric": {metric}, "resourceId": {resID}}
	for _, k := range []struct{ param, query string }{{"StartTime", "start"}, {"EndTime", "end"}} {
		v, err := c.p.str(k.param)
		if err != nil {
			return nil, err
		}
		if v != "" {
			q.Set(k.query, v)
		}
	}
	if c.p.has("Period") {
		period, err := c.p.integer("Period", 0, minMetricPeriodSeconds, maxMetricPeriodSeconds)
		if err != nil {
			return nil, err
		}
		q.Set("period", strconv.FormatInt(period, 10))
	}
	r, err := s.getResource(c, resID)
	if err != nil || r.ResourceID != resID || r.State == stateReleased {
		if err == nil || downstreamCode(err) == "Resource.NotFound" {
			return nil, errMetricResourceNotFound
		}
		return nil, err
	}
	series, err := s.metricData(c, q)
	if err != nil {
		switch downstreamCode(err) {
		case "Monitor.ResourceNotFound":
			return nil, errMetricResourceNotFound
		case "Common.InvalidParameter":
			var de *downstreamError
			errors.As(err, &de)
			msg := metricParamNames.ReplaceAllStringFunc(de.Message, func(m string) string { return metricParamRename[m] })
			return nil, errorsx.New(codeInvalidParameter, http.StatusBadRequest,
				nonEmpty(msg, "A parameter is not valid."))
		}
		return nil, err
	}
	if series.Metric != metric || series.ResourceID != resID {
		return nil, contractError(s.monitor, fmt.Sprintf("metrics answered %q/%q for %q/%q",
			series.Metric, series.ResourceID, metric, resID))
	}
	points := make([]map[string]any, 0, len(series.Points))
	for _, pt := range series.Points {
		points = append(points, map[string]any{"Timestamp": pt.Timestamp, "Value": pt.Value})
	}
	return map[string]any{
		"MetricName": series.Metric,
		"ResourceId": series.ResourceID,
		"Period":     series.Period,
		"Datapoints": points,
		"Source":     series.Source,
	}, nil
}
