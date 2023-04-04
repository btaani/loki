package logql

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/grafana/loki/pkg/logqlmodel/metadata"
	"github.com/grafana/loki/pkg/querier/queryrange/queryrangebase/definitions"

	"github.com/go-kit/log"
	json "github.com/json-iterator/go"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	promql_parser "github.com/prometheus/prometheus/promql/parser"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/weaveworks/common/user"

	"github.com/grafana/loki/pkg/iter"
	"github.com/grafana/loki/pkg/logproto"
	"github.com/grafana/loki/pkg/logql/syntax"
	"github.com/grafana/loki/pkg/logqlmodel"
	"github.com/grafana/loki/pkg/logqlmodel/stats"
	"github.com/grafana/loki/pkg/util"
	"github.com/grafana/loki/pkg/util/httpreq"
)

var (
	testSize        = int64(300)
	ErrMock         = errors.New("mock error")
	ErrMockMultiple = util.MultiError{ErrMock, ErrMock}
)

func TestEngine_LogsRateUnwrap(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		qs        string
		ts        time.Time
		direction logproto.Direction
		limit     uint32

		// an array of data per params will be returned by the querier.
		// This is to cover logql that requires multiple queries.
		data   interface{}
		params interface{}

		expected interface{}
	}{
		{
			`rate({app="foo"} | unwrap foo [30s])`,
			time.Unix(60, 0),
			logproto.FORWARD,
			10,
			// create a stream {app="foo"} with 300 samples starting at 46s and ending at 345s with a constant value of 1
			[][]logproto.Series{
				// 30s range the lower bound of the range is not inclusive only 15 samples will make it 60 included
				{newSeries(testSize, offset(46, constantValue(1)), `{app="foo"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(30, 0), End: time.Unix(60, 0), Selector: `rate({app="foo"} | unwrap foo[30s])`}},
			},
			// there are 15 samples (from 47 to 61) matched from the generated series
			// SUM(n=47, 61, 1) = 15
			// 15 / 30 = 0.5
			promql.Vector{promql.Sample{Point: promql.Point{T: 60 * 1000, V: 0.5}, Metric: labels.Labels{labels.Label{Name: "app", Value: "foo"}}}},
		},
		{
			`rate({app="foo"} | unwrap foo [30s])`,
			time.Unix(60, 0),
			logproto.FORWARD,
			10,
			// create a stream {app="foo"} with 300 samples starting at 46s and ending at 345s with an increasing value by 1
			[][]logproto.Series{
				// 30s range the lower bound of the range is not inclusive only 15 samples will make it 60 included
				{newSeries(testSize, offset(46, incValue(1)), `{app="foo"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(30, 0), End: time.Unix(60, 0), Selector: `rate({app="foo"} | unwrap foo[30s])`}},
			},
			// there are 15 samples (from 47 to 61) matched from the generated series
			// SUM(n=47, 61, n) = (47+48+...+61) = 810
			// 810 / 30 = 27
			promql.Vector{promql.Sample{Point: promql.Point{T: 60 * 1000, V: 27}, Metric: labels.Labels{labels.Label{Name: "app", Value: "foo"}}}},
		},
		{
			`rate_counter({app="foo"} | unwrap foo [30s])`,
			time.Unix(60, 0),
			logproto.FORWARD,
			10,
			// create a stream {app="foo"} with 300 samples starting at 46s and ending at 345s with a constant value of 1
			[][]logproto.Series{
				// 30s range the lower bound of the range is not inclusive only 15 samples will make it 60 included
				{newSeries(testSize, offset(46, constantValue(1)), `{app="foo"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(30, 0), End: time.Unix(60, 0), Selector: `rate_counter({app="foo"} | unwrap foo[30s])`}},
			},
			// there are 15 samples (from 47 to 61) matched from the generated series
			// (1 - 1) / 30 = 0
			promql.Vector{promql.Sample{Point: promql.Point{T: 60 * 1000, V: 0}, Metric: labels.Labels{labels.Label{Name: "app", Value: "foo"}}}},
		},
		{
			`rate_counter({app="foo"} | unwrap foo [30s])`,
			time.Unix(60, 0),
			logproto.FORWARD,
			10,
			// create a stream {app="foo"} with 300 samples starting at 46s and ending at 345s with an increasing value by 1
			[][]logproto.Series{
				// 30s range the lower bound of the range is not inclusive only 15 samples will make it 60 included
				{newSeries(testSize, offset(46, incValue(1)), `{app="foo"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(30, 0), End: time.Unix(60, 0), Selector: `rate_counter({app="foo"} | unwrap foo[30s])`}},
			},
			// there are 15 samples (from 47 to 61) matched from the generated series
			// (61 - 47) / 30 = 0.4666
			promql.Vector{promql.Sample{Point: promql.Point{T: 60 * 1000, V: 0.46666766666666665}, Metric: labels.Labels{labels.Label{Name: "app", Value: "foo"}}}},
		},
	} {
		test := test
		t.Run(fmt.Sprintf("%s %s", test.qs, test.direction), func(t *testing.T) {
			t.Parallel()

			eng := NewEngine(EngineOpts{}, newQuerierRecorder(t, test.data, test.params), NoLimits, log.NewNopLogger())
			q := eng.Query(LiteralParams{
				qs:        test.qs,
				start:     test.ts,
				end:       test.ts,
				direction: test.direction,
				limit:     test.limit,
			})
			res, err := q.Exec(user.InjectOrgID(context.Background(), "fake"))
			if expectedError, ok := test.expected.(error); ok {
				assert.Equal(t, expectedError.Error(), err.Error())
			} else {
				if err != nil {
					t.Fatal(err)
				}
				assert.Equal(t, test.expected, res.Data)
			}
		})
	}
}

func TestEngine_LogsInstantQuery(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		qs        string
		ts        time.Time
		direction logproto.Direction
		limit     uint32

		// an array of data per params will be returned by the querier.
		// This is to cover logql that requires multiple queries.
		data   interface{}
		params interface{}

		expected interface{}
	}{
		{
			`{app="foo"}`, time.Unix(30, 0), logproto.FORWARD, 10,
			[][]logproto.Stream{
				{newStream(testSize, identity, `{app="foo"}`)},
			},
			[]SelectLogParams{
				{&logproto.QueryRequest{Direction: logproto.FORWARD, Start: time.Unix(0, 0), End: time.Unix(30, 0), Limit: 10, Selector: `{app="foo"}`}},
			},
			logqlmodel.Streams([]logproto.Stream{newStream(10, identity, `{app="foo"}`)}),
		},
		{
			`{app="bar"} |= "foo" |~ ".+bar"`, time.Unix(30, 0), logproto.BACKWARD, 30,
			[][]logproto.Stream{
				{newStream(testSize, identity, `{app="bar"}`)},
			},
			[]SelectLogParams{
				{&logproto.QueryRequest{Direction: logproto.BACKWARD, Start: time.Unix(0, 0), End: time.Unix(30, 0), Limit: 30, Selector: `{app="bar"}|="foo"|~".+bar"`}},
			},
			logqlmodel.Streams([]logproto.Stream{newStream(30, identity, `{app="bar"}`)}),
		},
		{
			`rate({app="foo"} |~".+bar" [1m])`, time.Unix(60, 0), logproto.BACKWARD, 10,
			[][]logproto.Series{
				{newSeries(testSize, identity, `{app="foo"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `rate({app="foo"}|~".+bar"[1m])`}},
			},
			promql.Vector{promql.Sample{Point: promql.Point{T: 60 * 1000, V: 1}, Metric: labels.Labels{labels.Label{Name: "app", Value: "foo"}}}},
		},
		{
			`rate({app="foo"}[30s])`, time.Unix(60, 0), logproto.FORWARD, 10,
			[][]logproto.Series{
				// 30s range the lower bound of the range is not inclusive only 15 samples will make it 60 included
				{newSeries(testSize, offset(46, identity), `{app="foo"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(30, 0), End: time.Unix(60, 0), Selector: `rate({app="foo"}[30s])`}},
			},
			promql.Vector{promql.Sample{Point: promql.Point{T: 60 * 1000, V: 0.5}, Metric: labels.Labels{labels.Label{Name: "app", Value: "foo"}}}},
		},
		{
			`rate({app="foo"} | unwrap foo [30s])`, time.Unix(60, 0), logproto.FORWARD, 10,
			[][]logproto.Series{
				// 30s range the lower bound of the range is not inclusive only 15 samples will make it 60 included
				{newSeries(testSize, offset(46, constantValue(2)), `{app="foo"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(30, 0), End: time.Unix(60, 0), Selector: `rate({app="foo"} | unwrap foo[30s])`}},
			},
			// SUM(n=46, 61, 2) = 30
			// 30 / 30 = 1
			promql.Vector{promql.Sample{Point: promql.Point{T: 60 * 1000, V: 1.0}, Metric: labels.Labels{labels.Label{Name: "app", Value: "foo"}}}},
		},
		{
			`count_over_time({app="foo"} |~".+bar" [1m])`, time.Unix(60, 0), logproto.BACKWARD, 10,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`)}, // 10 , 20 , 30 .. 60 = 6 total
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `count_over_time({app="foo"}|~".+bar"[1m])`}},
			},
			promql.Vector{promql.Sample{Point: promql.Point{T: 60 * 1000, V: 6}, Metric: labels.Labels{labels.Label{Name: "app", Value: "foo"}}}},
		},
		{
			`first_over_time({app="foo"} |~".+bar" | unwrap foo [1m])`, time.Unix(60, 0), logproto.BACKWARD, 10,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`)}, // 10 , 20 , 30 .. 60 = 6 total
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `first_over_time({app="foo"}|~".+bar"| unwrap foo [1m])`}},
			},
			promql.Vector{promql.Sample{Point: promql.Point{T: 60 * 1000, V: 1}, Metric: labels.Labels{labels.Label{Name: "app", Value: "foo"}}}},
		},
		{
			`count_over_time({app="foo"} |~".+bar" [1m] offset 30s)`, time.Unix(90, 0), logproto.BACKWARD, 10,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`)}, // 10 , 20 , 30 .. 60 = 6 total
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `count_over_time({app="foo"}|~".+bar"[1m] offset 30s)`}},
			},
			promql.Vector{promql.Sample{Point: promql.Point{T: 90 * 1000, V: 6}, Metric: labels.Labels{labels.Label{Name: "app", Value: "foo"}}}},
		},
		{
			`count_over_time(({app="foo"} |~".+bar")[5m])`, time.Unix(5*60, 0), logproto.BACKWARD, 10,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`)}, // 10 , 20 , 30 .. 300 = 30 total
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(5*60, 0), Selector: `count_over_time({app="foo"}|~".+bar"[5m])`}},
			},
			promql.Vector{promql.Sample{Point: promql.Point{T: 5 * 60 * 1000, V: 30}, Metric: labels.Labels{labels.Label{Name: "app", Value: "foo"}}}},
		},
		{
			`absent_over_time(({app="foo"} |~".+bar")[5m])`, time.Unix(5*60, 0), logproto.BACKWARD, 10,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`)}, // 10 , 20 , 30 .. 300 = 30 total
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(5*60, 0), Selector: `absent_over_time({app="foo"}|~".+bar"[5m])`}},
			},
			promql.Vector{},
		},
		{
			`absent_over_time(({app="foo"} |~".+bar")[5m])`, time.Unix(5*60, 0), logproto.BACKWARD, 10,
			[][]logproto.Series{},
			[]SelectSampleParams{},
			promql.Vector{promql.Sample{Point: promql.Point{T: 5 * 60 * 1000, V: 1}, Metric: labels.Labels{labels.Label{Name: "app", Value: "foo"}}}},
		},
		{
			`avg(count_over_time({app=~"foo|bar"} |~".+bar" [1m]))`, time.Unix(60, 0), logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(10, identity), `{app="foo"}`),
					newSeries(testSize, factor(10, identity), `{app="bar"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `count_over_time({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Vector{
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 6}, Metric: labels.Labels{}},
			},
		},
		{
			`min(rate({app=~"foo|bar"} |~".+bar" [1m]))`, time.Unix(60, 0), logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, factor(10, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `rate({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Vector{
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 0.1}, Metric: labels.Labels{}},
			},
		},
		{
			`max by (app) (rate({app=~"foo|bar"} |~".+bar" [1m]))`, time.Unix(60, 0), logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, factor(5, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `rate({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Vector{
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 0.2}, Metric: labels.Labels{labels.Label{Name: "app", Value: "bar"}}},
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 0.1}, Metric: labels.Labels{labels.Label{Name: "app", Value: "foo"}}},
			},
		},
		{
			`max(rate({app=~"foo|bar"} |~".+bar" [1m]))`, time.Unix(60, 0), logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, factor(5, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `rate({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Vector{
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 0.2}, Metric: labels.Labels{}},
			},
		},
		{
			`sum(rate({app=~"foo|bar"} |~".+bar" [1m]))`, time.Unix(60, 0), logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(5, identity), `{app="foo"}`), newSeries(testSize, factor(5, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum(rate({app=~"foo|bar"} |~".+bar" [1m]))`}},
			},
			promql.Vector{
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 0.4}, Metric: labels.Labels{}},
			},
		},
		{
			`sum(count_over_time({app=~"foo|bar"} |~".+bar" [1m])) by (app)`, time.Unix(60, 0), logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, factor(10, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (app)(count_over_time({app=~"foo|bar"} |~".+bar" [1m]))`}},
			},
			promql.Vector{
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 6}, Metric: labels.Labels{labels.Label{Name: "app", Value: "bar"}}},
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 6}, Metric: labels.Labels{labels.Label{Name: "app", Value: "foo"}}},
			},
		},
		{
			`sum(count_over_time({app=~"foo|bar"} |~".+bar" [1m])) by (namespace,app)`, time.Unix(60, 0), logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(10, identity), `{app="foo", namespace="a"}`),
					newSeries(testSize, factor(10, identity), `{app="bar", namespace="b"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (namespace,app) (count_over_time({app=~"foo|bar"} |~".+bar" [1m])) `}},
			},
			promql.Vector{
				promql.Sample{
					Point: promql.Point{T: 60 * 1000, V: 6},
					Metric: labels.Labels{
						labels.Label{Name: "app", Value: "bar"},
						labels.Label{Name: "namespace", Value: "b"},
					},
				},
				promql.Sample{
					Point: promql.Point{T: 60 * 1000, V: 6},
					Metric: labels.Labels{
						labels.Label{Name: "app", Value: "foo"},
						labels.Label{Name: "namespace", Value: "a"},
					},
				},
			},
		},
		{
			`sum(count_over_time({app=~"foo|bar"} |~".+bar" [1m] offset 30s)) by (namespace,app)`, time.Unix(90, 0), logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(10, identity), `{app="foo", namespace="a"}`),
					newSeries(testSize, factor(10, identity), `{app="bar", namespace="b"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (namespace,app) (count_over_time({app=~"foo|bar"} |~".+bar" [1m] offset 30s)) `}},
			},
			promql.Vector{
				promql.Sample{
					Point: promql.Point{T: 90 * 1000, V: 6},
					Metric: labels.Labels{
						labels.Label{Name: "app", Value: "bar"},
						labels.Label{Name: "namespace", Value: "b"},
					},
				},
				promql.Sample{
					Point: promql.Point{T: 90 * 1000, V: 6},
					Metric: labels.Labels{
						labels.Label{Name: "app", Value: "foo"},
						labels.Label{Name: "namespace", Value: "a"},
					},
				},
			},
		},
		{
			`label_replace(
				sum(count_over_time({app=~"foo|bar"} |~".+bar" [1m])) by (namespace,app),
				"new",
				"$1",
				"app",
				"f(.*)"
				)`, time.Unix(60, 0), logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(10, identity), `{app="foo", namespace="a"}`),
					newSeries(testSize, factor(10, identity), `{app="bar", namespace="b"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (namespace,app) (count_over_time({app=~"foo|bar"} |~".+bar" [1m])) `}},
			},
			promql.Vector{
				promql.Sample{
					Point: promql.Point{T: 60 * 1000, V: 6},
					Metric: labels.Labels{
						labels.Label{Name: "app", Value: "bar"},
						labels.Label{Name: "namespace", Value: "b"},
					},
				},
				promql.Sample{
					Point: promql.Point{T: 60 * 1000, V: 6},
					Metric: labels.Labels{
						labels.Label{Name: "app", Value: "foo"},
						labels.Label{Name: "namespace", Value: "a"},
						labels.Label{Name: "new", Value: "oo"},
					},
				},
			},
		},
		{
			`count(count_over_time({app=~"foo|bar"} |~".+bar" [1m])) without (app)`, time.Unix(60, 0), logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, factor(10, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `count_over_time({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Vector{
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 2}, Metric: labels.Labels{}},
			},
		},
		{
			`stdvar without (app) (count_over_time(({app=~"foo|bar"} |~".+bar")[1m])) `, time.Unix(60, 0), logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, factor(5, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `count_over_time({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Vector{
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 9}, Metric: labels.Labels{}},
			},
		},
		{
			`stddev(count_over_time(({app=~"foo|bar"} |~".+bar")[1m])) `, time.Unix(60, 0), logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, factor(2, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `count_over_time({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Vector{
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 12}, Metric: labels.Labels{}},
			},
		},
		{
			`rate(({app=~"foo|bar"} |~".+bar")[1m])`, time.Unix(60, 0), logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, offset(46, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `rate({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Vector{
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 0.25}, Metric: labels.Labels{labels.Label{Name: "app", Value: "bar"}}},
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 0.1}, Metric: labels.Labels{labels.Label{Name: "app", Value: "foo"}}},
			},
		},
		{
			`topk(2,rate(({app=~"foo|bar"} |~".+bar")[1m]))`, time.Unix(60, 0), logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, offset(46, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `rate({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Vector{
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 0.25}, Metric: labels.Labels{labels.Label{Name: "app", Value: "bar"}}},
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 0.1}, Metric: labels.Labels{labels.Label{Name: "app", Value: "foo"}}},
			},
		},
		{
			`topk(1,rate(({app=~"foo|bar"} |~".+bar")[1m]))`, time.Unix(60, 0), logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, offset(46, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `rate({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Vector{
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 0.25}, Metric: labels.Labels{labels.Label{Name: "app", Value: "bar"}}},
			},
		},

		{
			`topk(1,rate(({app=~"foo|bar"} |~".+bar")[1m])) by (app)`, time.Unix(60, 0), logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, offset(46, identity), `{app="bar"}`),
					newSeries(testSize, factor(5, identity), `{app="fuzz"}`), newSeries(testSize, identity, `{app="buzz"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `rate({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Vector{
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 0.25}, Metric: labels.Labels{labels.Label{Name: "app", Value: "bar"}}},
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 1}, Metric: labels.Labels{labels.Label{Name: "app", Value: "buzz"}}},
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 0.1}, Metric: labels.Labels{labels.Label{Name: "app", Value: "foo"}}},
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 0.2}, Metric: labels.Labels{labels.Label{Name: "app", Value: "fuzz"}}},
			},
		},
		{
			`bottomk(2,rate(({app=~"foo|bar"} |~".+bar")[1m]))`, time.Unix(60, 0), logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, offset(46, identity), `{app="bar"}`),
					newSeries(testSize, factor(5, identity), `{app="fuzz"}`), newSeries(testSize, identity, `{app="buzz"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `rate({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Vector{
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 0.1}, Metric: labels.Labels{labels.Label{Name: "app", Value: "foo"}}},
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 0.2}, Metric: labels.Labels{labels.Label{Name: "app", Value: "fuzz"}}},
			},
		},
		{
			`bottomk(3,rate(({app=~"foo|bar"} |~".+bar")[1m])) without (app)`, time.Unix(60, 0), logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, offset(46, identity), `{app="bar"}`),
					newSeries(testSize, factor(5, identity), `{app="fuzz"}`), newSeries(testSize, identity, `{app="buzz"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `rate({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Vector{
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 0.25}, Metric: labels.Labels{labels.Label{Name: "app", Value: "bar"}}},
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 0.1}, Metric: labels.Labels{labels.Label{Name: "app", Value: "foo"}}},
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 0.2}, Metric: labels.Labels{labels.Label{Name: "app", Value: "fuzz"}}},
			},
		},
		{
			`bottomk(3,rate(({app=~"foo|bar"} |~".+bar")[1m])) without (app) + 1`, time.Unix(60, 0), logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, offset(46, identity), `{app="bar"}`),
					newSeries(testSize, factor(5, identity), `{app="fuzz"}`), newSeries(testSize, identity, `{app="buzz"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `rate({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Vector{
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 1.25}, Metric: labels.Labels{labels.Label{Name: "app", Value: "bar"}}},
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 1.1}, Metric: labels.Labels{labels.Label{Name: "app", Value: "foo"}}},
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 1.2}, Metric: labels.Labels{labels.Label{Name: "app", Value: "fuzz"}}},
			},
		},
		//sort and sort_desc
		{
			`sort(rate(({app=~"foo|bar"} |~".+bar")[1m]))  + 1`, time.Unix(60, 0), logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, offset(46, identity), `{app="bar"}`),
					newSeries(testSize, factor(5, identity), `{app="fuzz"}`), newSeries(testSize, identity, `{app="buzz"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `rate({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Vector{
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 1.1}, Metric: labels.Labels{labels.Label{Name: "app", Value: "foo"}}},
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 1.2}, Metric: labels.Labels{labels.Label{Name: "app", Value: "fuzz"}}},
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 1.25}, Metric: labels.Labels{labels.Label{Name: "app", Value: "bar"}}},
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 2}, Metric: labels.Labels{labels.Label{Name: "app", Value: "buzz"}}},
			},
		},
		{
			`sort_desc(rate(({app=~"foo|bar"} |~".+bar")[1m]))  + 1`, time.Unix(60, 0), logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, offset(46, identity), `{app="bar"}`),
					newSeries(testSize, factor(5, identity), `{app="fuzz"}`), newSeries(testSize, identity, `{app="buzz"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `rate({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Vector{
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 2}, Metric: labels.Labels{labels.Label{Name: "app", Value: "buzz"}}},
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 1.25}, Metric: labels.Labels{labels.Label{Name: "app", Value: "bar"}}},
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 1.2}, Metric: labels.Labels{labels.Label{Name: "app", Value: "fuzz"}}},
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 1.1}, Metric: labels.Labels{labels.Label{Name: "app", Value: "foo"}}},
			},
		},
		{
			// healthcheck
			`1+1`, time.Unix(60, 0), logproto.FORWARD, 100,
			nil,
			nil,
			promql.Scalar{T: 60 * 1000, V: 2},
		},
		{
			// single literal
			`2`,
			time.Unix(60, 0), logproto.FORWARD, 100,
			nil,
			nil,
			promql.Scalar{T: 60 * 1000, V: 2},
		},
		{
			// vector instant
			`vector(2)`,
			time.Unix(60, 0), logproto.FORWARD, 100,
			nil,
			nil,
			promql.Vector{promql.Sample{
				Point:  promql.Point{T: 60 * 1000, V: 2},
				Metric: labels.Labels{},
			}},
		},
		{
			// single comparison
			`1 == 1`,
			time.Unix(60, 0), logproto.FORWARD, 100,
			nil,
			nil,
			promql.Scalar{T: 60 * 1000, V: 1},
		},
		{
			// single comparison, reduce away bool modifier between scalars
			`1 == bool 1`,
			time.Unix(60, 0), logproto.FORWARD, 100,
			nil,
			nil,
			promql.Scalar{T: 60 * 1000, V: 1},
		},
		{
			`count_over_time({app="foo"}[1m]) > 1`,
			time.Unix(60, 0),
			logproto.FORWARD,
			0,
			[][]logproto.Series{
				{newSeries(testSize, identity, `{app="foo"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `count_over_time({app="foo"}[1m])`}},
			},
			promql.Vector{
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 60}, Metric: labels.Labels{labels.Label{Name: "app", Value: "foo"}}},
			},
		},
		{
			`count_over_time({app="foo"}[1m]) > count_over_time({app="bar"}[1m])`,
			time.Unix(60, 0),
			logproto.FORWARD,
			0,
			[][]logproto.Series{
				{newSeries(testSize, identity, `{app="foo"}`)},
				{},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `count_over_time({app="foo"}[1m])`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `count_over_time({app="bar"}[1m])`}},
			},
			promql.Vector{},
		},
		{
			`count_over_time({app="foo"}[1m]) > bool count_over_time({app="bar"}[1m])`,
			time.Unix(60, 0),
			logproto.FORWARD,
			0,
			[][]logproto.Series{
				{newSeries(testSize, identity, `{app="foo"}`)},
				{},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `count_over_time({app="foo"}[1m])`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `count_over_time({app="bar"}[1m])`}},
			},
			promql.Vector{},
		},
		{
			`sum without(app) (count_over_time({app="foo"}[1m])) > bool sum without(app) (count_over_time({app="bar"}[1m]))`,
			time.Unix(60, 0),
			logproto.FORWARD,
			0,
			[][]logproto.Series{
				{newSeries(testSize, identity, `{app="foo"}`)},
				{},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum without (app) (count_over_time({app="foo"}[1m]))`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum without (app) (count_over_time({app="bar"}[1m]))`}},
			},
			promql.Vector{},
		},
		{
			`sum without(app) (count_over_time({app="foo"}[1m])) >= sum without(app) (count_over_time({app="bar"}[1m]))`,
			time.Unix(60, 0),
			logproto.FORWARD,
			0,
			[][]logproto.Series{
				{newSeries(testSize, identity, `{app="foo"}`)},
				{newSeries(testSize, identity, `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum without(app) (count_over_time({app="foo"}[1m]))`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum without(app) (count_over_time({app="bar"}[1m]))`}},
			},
			promql.Vector{
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 60}, Metric: labels.Labels{}},
			},
		},
		{
			`10 / 5 / 2`,
			time.Unix(60, 0),
			logproto.FORWARD,
			0,
			nil,
			nil,
			promql.Scalar{T: 60 * 1000, V: 1},
		},
		{
			`10 / (5 / 2)`,
			time.Unix(60, 0),
			logproto.FORWARD,
			0,
			nil,
			nil,
			promql.Scalar{T: 60 * 1000, V: 4},
		},
		{
			`10 / ((rate({app="foo"} |~".+bar" [1m]) /5))`, time.Unix(60, 0), logproto.BACKWARD, 10,
			[][]logproto.Series{
				{newSeries(testSize, identity, `{app="foo"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `rate({app="foo"}|~".+bar"[1m])`}},
			},
			promql.Vector{promql.Sample{Point: promql.Point{T: 60 * 1000, V: 50}, Metric: labels.Labels{labels.Label{Name: "app", Value: "foo"}}}},
		},
		{
			`sum by (app) (count_over_time({app="foo"}[1m])) + sum by (app) (count_over_time({app="bar"}[1m]))`,
			time.Unix(60, 0),
			logproto.FORWARD,
			0,
			[][]logproto.Series{
				{newSeries(testSize, identity, `{app="foo"}`)},
				{newSeries(testSize, identity, `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (app) (count_over_time({app="foo"}[1m]))`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (app) (count_over_time({app="bar"}[1m]))`}},
			},
			promql.Vector{},
		},
		{
			`sum by (app) (count_over_time({app="foo"}[1m])) + sum by (app) (count_over_time({app="foo"}[1m]))`,
			time.Unix(60, 0),
			logproto.FORWARD,
			0,
			[][]logproto.Series{
				{newSeries(testSize, identity, `{app="foo"}`)},
				{newSeries(testSize, identity, `{app="foo"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (app) (count_over_time({app="foo"}[1m]))`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (app) (count_over_time({app="foo"}[1m]))`}},
			},
			promql.Vector{
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 120}, Metric: labels.Labels{labels.Label{Name: "app", Value: "foo"}}},
			},
		},
		{
			`sum by (app,machine) (count_over_time({app="foo"}[1m])) + on () sum by (app) (count_over_time({app="foo"}[1m]))`,
			time.Unix(60, 0),
			logproto.FORWARD,
			0,
			[][]logproto.Series{
				{newSeries(testSize, identity, `{app="foo",machine="fuzz"}`)},
				{newSeries(testSize, identity, `{app="foo"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (app,machine) (count_over_time({app="foo"}[1m]))`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (app) (count_over_time({app="foo"}[1m]))`}},
			},
			promql.Vector{
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 120}, Metric: labels.Labels{}},
			},
		},
		{
			`sum by (app,machine) (count_over_time({app="foo"}[1m])) + on (app) sum by (app) (count_over_time({app="foo"}[1m]))`,
			time.Unix(60, 0),
			logproto.FORWARD,
			0,
			[][]logproto.Series{
				{newSeries(testSize, identity, `{app="foo",machine="fuzz"}`)},
				{newSeries(testSize, identity, `{app="foo"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (app,machine) (count_over_time({app="foo"}[1m]))`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (app) (count_over_time({app="foo"}[1m]))`}},
			},
			promql.Vector{
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 120}, Metric: labels.Labels{labels.Label{Name: "app", Value: "foo"}}},
			},
		},
		{
			`sum by (app,machine) (count_over_time({app="foo"}[1m])) > bool ignoring (machine) sum by (app) (count_over_time({app="foo"}[1m]))`,
			time.Unix(60, 0),
			logproto.FORWARD,
			0,
			[][]logproto.Series{
				{newSeries(testSize, identity, `{app="foo",machine="fuzz"}`)},
				{newSeries(testSize, identity, `{app="foo"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (app,machine) (count_over_time({app="foo"}[1m]))`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (app) (count_over_time({app="foo"}[1m]))`}},
			},
			promql.Vector{
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 0}, Metric: labels.Labels{labels.Label{Name: "app", Value: "foo"}}},
			},
		},
		{
			`sum by (app,machine) (count_over_time({app="foo"}[1m])) > bool ignoring (machine) sum by (app) (count_over_time({app="foo"}[1m]))`,
			time.Unix(60, 0),
			logproto.FORWARD,
			0,
			[][]logproto.Series{
				{newSeries(testSize, identity, `{app="foo",machine="fuzz"}`), newSeries(testSize, identity, `{app="foo",machine="buzz"}`)},
				{newSeries(testSize, identity, `{app="foo"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (app,machine) (count_over_time({app="foo"}[1m]))`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (app) (count_over_time({app="foo"}[1m]))`}},
			},
			errors.New("multiple matches for labels: many-to-one matching must be explicit (group_left/group_right)"),
		},
		{
			`sum by (app,machine) (count_over_time({app="foo"}[1m])) > bool on () group_left sum by (app) (count_over_time({app="foo"}[1m]))`,
			time.Unix(60, 0),
			logproto.FORWARD,
			0,
			[][]logproto.Series{
				{newSeries(testSize, identity, `{app="foo",machine="fuzz"}`), newSeries(testSize, identity, `{app="foo",machine="buzz"}`)},
				{newSeries(testSize, identity, `{app="foo"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (app,machine) (count_over_time({app="foo"}[1m]))`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (app) (count_over_time({app="foo"}[1m]))`}},
			},
			promql.Vector{
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 0}, Metric: labels.Labels{labels.Label{Name: "app", Value: "foo"}, labels.Label{Name: "machine", Value: "buzz"}}},
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 0}, Metric: labels.Labels{labels.Label{Name: "app", Value: "foo"}, labels.Label{Name: "machine", Value: "fuzz"}}},
			},
		},
		{
			`sum by (app,machine) (count_over_time({app="foo"}[1m])) > bool on () group_left () sum by (app) (count_over_time({app="foo"}[1m]))`,
			time.Unix(60, 0),
			logproto.FORWARD,
			0,
			[][]logproto.Series{
				{newSeries(testSize, identity, `{app="foo",machine="fuzz"}`), newSeries(testSize, identity, `{app="foo",machine="buzz"}`)},
				{newSeries(testSize, identity, `{app="foo"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (app,machine) (count_over_time({app="foo"}[1m]))`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (app) (count_over_time({app="foo"}[1m]))`}},
			},
			promql.Vector{
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 0}, Metric: labels.Labels{labels.Label{Name: "app", Value: "foo"}, labels.Label{Name: "machine", Value: "buzz"}}},
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 0}, Metric: labels.Labels{labels.Label{Name: "app", Value: "foo"}, labels.Label{Name: "machine", Value: "fuzz"}}},
			},
		},
		{
			`sum by (app,machine) (count_over_time({app="foo"}[1m])) > bool on (app) group_left (pool) sum by (app,pool) (count_over_time({app="foo"}[1m]))`,
			time.Unix(60, 0),
			logproto.FORWARD,
			0,
			[][]logproto.Series{
				{newSeries(testSize, identity, `{app="foo",machine="fuzz"}`), newSeries(testSize, identity, `{app="foo",machine="buzz"}`)},
				{newSeries(testSize, identity, `{app="foo",pool="foo"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (app,machine) (count_over_time({app="foo"}[1m]))`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (app,pool) (count_over_time({app="foo"}[1m]))`}},
			},
			promql.Vector{
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 0}, Metric: labels.Labels{labels.Label{Name: "app", Value: "foo"}, labels.Label{Name: "machine", Value: "buzz"}, labels.Label{Name: "pool", Value: "foo"}}},
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 0}, Metric: labels.Labels{labels.Label{Name: "app", Value: "foo"}, labels.Label{Name: "machine", Value: "fuzz"}, labels.Label{Name: "pool", Value: "foo"}}},
			},
		},
		{
			`sum by (app,pool) (count_over_time({app="foo"}[1m])) > bool on (app) group_right (pool) sum by (app,machine) (count_over_time({app="foo"}[1m]))`,
			time.Unix(60, 0),
			logproto.FORWARD,
			0,
			[][]logproto.Series{
				{newSeries(testSize, identity, `{app="foo",pool="foo"}`)},
				{newSeries(testSize, identity, `{app="foo",machine="fuzz"}`), newSeries(testSize, identity, `{app="foo",machine="buzz"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (app,pool) (count_over_time({app="foo"}[1m]))`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (app,machine) (count_over_time({app="foo"}[1m]))`}},
			},
			promql.Vector{
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 0}, Metric: labels.Labels{labels.Label{Name: "app", Value: "foo"}, labels.Label{Name: "machine", Value: "buzz"}, labels.Label{Name: "pool", Value: "foo"}}},
				promql.Sample{Point: promql.Point{T: 60 * 1000, V: 0}, Metric: labels.Labels{labels.Label{Name: "app", Value: "foo"}, labels.Label{Name: "machine", Value: "fuzz"}, labels.Label{Name: "pool", Value: "foo"}}},
			},
		},
	} {
		test := test
		t.Run(fmt.Sprintf("%s %s", test.qs, test.direction), func(t *testing.T) {
			t.Parallel()

			eng := NewEngine(EngineOpts{}, newQuerierRecorder(t, test.data, test.params), NoLimits, log.NewNopLogger())
			q := eng.Query(LiteralParams{
				qs:        test.qs,
				start:     test.ts,
				end:       test.ts,
				direction: test.direction,
				limit:     test.limit,
			})
			res, err := q.Exec(user.InjectOrgID(context.Background(), "fake"))
			if expectedError, ok := test.expected.(error); ok {
				assert.Equal(t, expectedError.Error(), err.Error())
			} else {
				if err != nil {
					t.Fatal(err)
				}
				assert.Equal(t, test.expected, res.Data)
			}
		})
	}
}

func TestEngine_RangeQuery(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		qs        string
		start     time.Time
		end       time.Time
		step      time.Duration
		interval  time.Duration
		direction logproto.Direction
		limit     uint32

		// an array of streams per SelectParams will be returned by the querier.
		// This is to cover logql that requires multiple queries.
		data   interface{}
		params interface{}

		expected promql_parser.Value
	}{
		{
			`{app="foo"}`, time.Unix(0, 0), time.Unix(30, 0), time.Second, 0, logproto.FORWARD, 10,
			[][]logproto.Stream{
				{newStream(testSize, identity, `{app="foo"}`)},
			},
			[]SelectLogParams{
				{&logproto.QueryRequest{Direction: logproto.FORWARD, Start: time.Unix(0, 0), End: time.Unix(30, 0), Limit: 10, Selector: `{app="foo"}`}},
			},
			logqlmodel.Streams([]logproto.Stream{newStream(10, identity, `{app="foo"}`)}),
		},
		{
			`{app="food"}`, time.Unix(0, 0), time.Unix(30, 0), 0, 2 * time.Second, logproto.FORWARD, 10,
			[][]logproto.Stream{
				{newStream(testSize, identity, `{app="food"}`)},
			},
			[]SelectLogParams{
				{&logproto.QueryRequest{Direction: logproto.FORWARD, Start: time.Unix(0, 0), End: time.Unix(30, 0), Limit: 10, Selector: `{app="food"}`}},
			},
			logqlmodel.Streams([]logproto.Stream{newIntervalStream(10, 2*time.Second, identity, `{app="food"}`)}),
		},
		{
			`{app="fed"}`, time.Unix(0, 0), time.Unix(30, 0), 0, 2 * time.Second, logproto.BACKWARD, 10,
			[][]logproto.Stream{
				{newBackwardStream(testSize, identity, `{app="fed"}`)},
			},
			[]SelectLogParams{
				{&logproto.QueryRequest{Direction: logproto.BACKWARD, Start: time.Unix(0, 0), End: time.Unix(30, 0), Limit: 10, Selector: `{app="fed"}`}},
			},
			logqlmodel.Streams([]logproto.Stream{newBackwardIntervalStream(testSize, 10, 2*time.Second, identity, `{app="fed"}`)}),
		},
		{
			`{app="bar"} |= "foo" |~ ".+bar"`, time.Unix(0, 0), time.Unix(30, 0), time.Second, 0, logproto.BACKWARD, 30,
			[][]logproto.Stream{
				{newStream(testSize, identity, `{app="bar"}`)},
			},
			[]SelectLogParams{
				{&logproto.QueryRequest{Direction: logproto.BACKWARD, Start: time.Unix(0, 0), End: time.Unix(30, 0), Limit: 30, Selector: `{app="bar"}|="foo"|~".+bar"`}},
			},
			logqlmodel.Streams([]logproto.Stream{newStream(30, identity, `{app="bar"}`)}),
		},
		{
			`{app="barf"} |= "foo" |~ ".+bar"`, time.Unix(0, 0), time.Unix(30, 0), 0, 3 * time.Second, logproto.BACKWARD, 30,
			[][]logproto.Stream{
				{newBackwardStream(testSize, identity, `{app="barf"}`)},
			},
			[]SelectLogParams{
				{&logproto.QueryRequest{Direction: logproto.BACKWARD, Start: time.Unix(0, 0), End: time.Unix(30, 0), Limit: 30, Selector: `{app="barf"}|="foo"|~".+bar"`}},
			},
			logqlmodel.Streams([]logproto.Stream{newBackwardIntervalStream(testSize, 30, 3*time.Second, identity, `{app="barf"}`)}),
		},
		{
			`rate({app="foo"} |~".+bar" [1m])`, time.Unix(60, 0), time.Unix(120, 0), time.Minute, 0, logproto.BACKWARD, 10,
			[][]logproto.Series{
				{newSeries(testSize, identity, `{app="foo"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(120, 0), Selector: `rate({app="foo"}|~".+bar"[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "foo"}},
					Points: []promql.Point{{T: 60 * 1000, V: 1}, {T: 120 * 1000, V: 1}},
				},
			},
		},
		{
			`rate({app="foo"}[30s])`, time.Unix(60, 0), time.Unix(120, 0), 15 * time.Second, 0, logproto.FORWARD, 10,
			[][]logproto.Series{
				{newSeries(testSize, factor(2, identity), `{app="foo"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(30, 0), End: time.Unix(120, 0), Selector: `rate({app="foo"}[30s])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "foo"}},
					Points: []promql.Point{{T: 60 * 1000, V: 0.5}, {T: 75 * 1000, V: 0.5}, {T: 90 * 1000, V: 0.5}, {T: 105 * 1000, V: 0.5}, {T: 120 * 1000, V: 0.5}},
				},
			},
		},
		{
			`count_over_time({app="foo"} |~".+bar" [1m])`, time.Unix(60, 0), time.Unix(120, 0), 30 * time.Second, 0, logproto.BACKWARD, 10,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`)}, // 10 , 20 , 30 .. 60 = 6 total
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(120, 0), Selector: `count_over_time({app="foo"}|~".+bar"[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "foo"}},
					Points: []promql.Point{{T: 60 * 1000, V: 6}, {T: 90 * 1000, V: 6}, {T: 120 * 1000, V: 6}},
				},
			},
		},
		{
			`count_over_time(({app="foo"} |~".+bar")[5m])`, time.Unix(5*60, 0), time.Unix(5*120, 0), 30 * time.Second, 0, logproto.BACKWARD, 10,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`)}, // 10 , 20 , 30 .. 300 = 30 total
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(5*120, 0), Selector: `count_over_time({app="foo"}|~".+bar"[5m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "foo"}},
					Points: []promql.Point{
						{T: 300 * 1000, V: 30},
						{T: 330 * 1000, V: 30},
						{T: 360 * 1000, V: 30},
						{T: 390 * 1000, V: 30},
						{T: 420 * 1000, V: 30},
						{T: 450 * 1000, V: 30},
						{T: 480 * 1000, V: 30},
						{T: 510 * 1000, V: 30},
						{T: 540 * 1000, V: 30},
						{T: 570 * 1000, V: 30},
						{T: 600 * 1000, V: 30},
					},
				},
			},
		},
		{
			`last_over_time(({app="foo"} |~".+bar" | unwrap foo)[5m])`, time.Unix(5*60, 0), time.Unix(5*120, 0), 30 * time.Second, 0, logproto.BACKWARD, 10,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`)}, // 10 , 20 , 30 .. 300 = 30 total
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(5*120, 0), Selector: `last_over_time({app="foo"}|~".+bar"| unwrap foo[5m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "foo"}},
					Points: []promql.Point{
						{T: 300 * 1000, V: 1},
						{T: 330 * 1000, V: 1},
						{T: 360 * 1000, V: 1},
						{T: 390 * 1000, V: 1},
						{T: 420 * 1000, V: 1},
						{T: 450 * 1000, V: 1},
						{T: 480 * 1000, V: 1},
						{T: 510 * 1000, V: 1},
						{T: 540 * 1000, V: 1},
						{T: 570 * 1000, V: 1},
						{T: 600 * 1000, V: 1},
					},
				},
			},
		},
		{
			`avg(count_over_time({app=~"foo|bar"} |~".+bar" [1m]))`, time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, factor(10, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `count_over_time({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{},
					Points: []promql.Point{{T: 60 * 1000, V: 6}, {T: 90 * 1000, V: 6}, {T: 120 * 1000, V: 6}, {T: 150 * 1000, V: 6}, {T: 180 * 1000, V: 6}},
				},
			},
		},
		{
			`min(rate({app=~"foo|bar"} |~".+bar" [1m]))`, time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, factor(10, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{},
					Points: []promql.Point{{T: 60 * 1000, V: 0.1}, {T: 90 * 1000, V: 0.1}, {T: 120 * 1000, V: 0.1}, {T: 150 * 1000, V: 0.1}, {T: 180 * 1000, V: 0.1}},
				},
			},
		},
		{
			`max by (app) (rate({app=~"foo|bar"} |~".+bar" [1m]))`, time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, factor(5, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "bar"}},
					Points: []promql.Point{{T: 60 * 1000, V: 0.2}, {T: 90 * 1000, V: 0.2}, {T: 120 * 1000, V: 0.2}, {T: 150 * 1000, V: 0.2}, {T: 180 * 1000, V: 0.2}},
				},
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "foo"}},
					Points: []promql.Point{{T: 60 * 1000, V: 0.1}, {T: 90 * 1000, V: 0.1}, {T: 120 * 1000, V: 0.1}, {T: 150 * 1000, V: 0.1}, {T: 180 * 1000, V: 0.1}},
				},
			},
		},
		{
			`max(rate({app=~"foo|bar"} |~".+bar" [1m]))`, time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, factor(5, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{},
					Points: []promql.Point{{T: 60 * 1000, V: 0.2}, {T: 90 * 1000, V: 0.2}, {T: 120 * 1000, V: 0.2}, {T: 150 * 1000, V: 0.2}, {T: 180 * 1000, V: 0.2}},
				},
			},
		},
		{
			`sum(rate({app=~"foo|bar"} |~".+bar" [1m]))`, time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(5, identity), `{app="foo"}`), newSeries(testSize, factor(5, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `sum(rate({app=~"foo|bar"} |~".+bar" [1m]))`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{},
					Points: []promql.Point{{T: 60 * 1000, V: 0.4}, {T: 90 * 1000, V: 0.4}, {T: 120 * 1000, V: 0.4}, {T: 150 * 1000, V: 0.4}, {T: 180 * 1000, V: 0.4}},
				},
			},
		},
		{
			`sum(count_over_time({app=~"foo|bar"} |~".+bar" [1m])) by (app)`, time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, factor(5, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `sum by (app) (count_over_time({app=~"foo|bar"} |~".+bar" [1m]))`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "bar"}},
					Points: []promql.Point{{T: 60 * 1000, V: 12}, {T: 90 * 1000, V: 12}, {T: 120 * 1000, V: 12}, {T: 150 * 1000, V: 12}, {T: 180 * 1000, V: 12}},
				},
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "foo"}},
					Points: []promql.Point{{T: 60 * 1000, V: 6}, {T: 90 * 1000, V: 6}, {T: 120 * 1000, V: 6}, {T: 150 * 1000, V: 6}, {T: 180 * 1000, V: 6}},
				},
			},
		},
		{
			`sum(count_over_time({app=~"foo|bar"} |~".+bar" [1m])) by (namespace,cluster, app)`, time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(10, identity), `{app="foo", cluster="b", namespace="a"}`),
					newSeries(testSize, factor(5, identity), `{app="bar", cluster="a", namespace="b"}`),
					newSeries(testSize, factor(5, identity), `{app="foo", cluster="a" ,namespace="a"}`),
					newSeries(testSize, factor(10, identity), `{app="bar", cluster="b" ,namespace="b"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `sum by (namespace,cluster, app)(count_over_time({app=~"foo|bar"} |~".+bar" [1m]))`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "bar"}, {Name: "cluster", Value: "a"}, {Name: "namespace", Value: "b"}},
					Points: []promql.Point{{T: 60 * 1000, V: 12}, {T: 90 * 1000, V: 12}, {T: 120 * 1000, V: 12}, {T: 150 * 1000, V: 12}, {T: 180 * 1000, V: 12}},
				},
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "bar"}, {Name: "cluster", Value: "b"}, {Name: "namespace", Value: "b"}},
					Points: []promql.Point{{T: 60 * 1000, V: 6}, {T: 90 * 1000, V: 6}, {T: 120 * 1000, V: 6}, {T: 150 * 1000, V: 6}, {T: 180 * 1000, V: 6}},
				},
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "foo"}, {Name: "cluster", Value: "a"}, {Name: "namespace", Value: "a"}},
					Points: []promql.Point{{T: 60 * 1000, V: 12}, {T: 90 * 1000, V: 12}, {T: 120 * 1000, V: 12}, {T: 150 * 1000, V: 12}, {T: 180 * 1000, V: 12}},
				},
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "foo"}, {Name: "cluster", Value: "b"}, {Name: "namespace", Value: "a"}},
					Points: []promql.Point{{T: 60 * 1000, V: 6}, {T: 90 * 1000, V: 6}, {T: 120 * 1000, V: 6}, {T: 150 * 1000, V: 6}, {T: 180 * 1000, V: 6}},
				},
			},
		},
		{
			`sum(count_over_time({app=~"foo|bar"} |~".+bar" [1m])) by (cluster, namespace, app)`, time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(10, identity), `{app="foo", cluster="b", namespace="a"}`),
					newSeries(testSize, factor(5, identity), `{app="bar", cluster="a", namespace="b"}`),
					newSeries(testSize, factor(5, identity), `{app="foo", cluster="a" ,namespace="a"}`),
					newSeries(testSize, factor(10, identity), `{app="bar", cluster="b" ,namespace="b"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `sum by (cluster, namespace, app) (count_over_time({app=~"foo|bar"} |~".+bar" [1m]))`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "bar"}, {Name: "cluster", Value: "a"}, {Name: "namespace", Value: "b"}},
					Points: []promql.Point{{T: 60 * 1000, V: 12}, {T: 90 * 1000, V: 12}, {T: 120 * 1000, V: 12}, {T: 150 * 1000, V: 12}, {T: 180 * 1000, V: 12}},
				},
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "bar"}, {Name: "cluster", Value: "b"}, {Name: "namespace", Value: "b"}},
					Points: []promql.Point{{T: 60 * 1000, V: 6}, {T: 90 * 1000, V: 6}, {T: 120 * 1000, V: 6}, {T: 150 * 1000, V: 6}, {T: 180 * 1000, V: 6}},
				},
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "foo"}, {Name: "cluster", Value: "a"}, {Name: "namespace", Value: "a"}},
					Points: []promql.Point{{T: 60 * 1000, V: 12}, {T: 90 * 1000, V: 12}, {T: 120 * 1000, V: 12}, {T: 150 * 1000, V: 12}, {T: 180 * 1000, V: 12}},
				},
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "foo"}, {Name: "cluster", Value: "b"}, {Name: "namespace", Value: "a"}},
					Points: []promql.Point{{T: 60 * 1000, V: 6}, {T: 90 * 1000, V: 6}, {T: 120 * 1000, V: 6}, {T: 150 * 1000, V: 6}, {T: 180 * 1000, V: 6}},
				},
			},
		},
		{
			`sum(count_over_time({app=~"foo|bar"} |~".+bar" [1m])) by (namespace, app)`, time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(10, identity), `{app="foo", cluster="b", namespace="a"}`),
					newSeries(testSize, factor(5, identity), `{app="bar", cluster="a", namespace="b"}`),
					newSeries(testSize, factor(5, identity), `{app="foo", cluster="a" ,namespace="a"}`),
					newSeries(testSize, factor(10, identity), `{app="bar", cluster="b" ,namespace="b"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `sum by (namespace, app)(count_over_time({app=~"foo|bar"} |~".+bar" [1m]))`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "bar"}, {Name: "namespace", Value: "b"}},
					Points: []promql.Point{{T: 60 * 1000, V: 18}, {T: 90 * 1000, V: 18}, {T: 120 * 1000, V: 18}, {T: 150 * 1000, V: 18}, {T: 180 * 1000, V: 18}},
				},
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "foo"}, {Name: "namespace", Value: "a"}},
					Points: []promql.Point{{T: 60 * 1000, V: 18}, {T: 90 * 1000, V: 18}, {T: 120 * 1000, V: 18}, {T: 150 * 1000, V: 18}, {T: 180 * 1000, V: 18}},
				},
			},
		},
		{
			`count(count_over_time({app=~"foo|bar"} |~".+bar" [1m])) without (app)`, time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, factor(10, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `count_over_time({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{},
					Points: []promql.Point{{T: 60 * 1000, V: 2}, {T: 90 * 1000, V: 2}, {T: 120 * 1000, V: 2}, {T: 150 * 1000, V: 2}, {T: 180 * 1000, V: 2}},
				},
			},
		},
		{
			`stdvar without (app) (count_over_time(({app=~"foo|bar"} |~".+bar")[1m])) `, time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, factor(5, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `count_over_time({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{},
					Points: []promql.Point{{T: 60 * 1000, V: 9}, {T: 90 * 1000, V: 9}, {T: 120 * 1000, V: 9}, {T: 150 * 1000, V: 9}, {T: 180 * 1000, V: 9}},
				},
			},
		},
		{
			`stddev(count_over_time(({app=~"foo|bar"} |~".+bar")[1m])) `, time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, factor(2, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `count_over_time({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{},
					Points: []promql.Point{{T: 60 * 1000, V: 12}, {T: 90 * 1000, V: 12}, {T: 120 * 1000, V: 12}, {T: 150 * 1000, V: 12}, {T: 180 * 1000, V: 12}},
				},
			},
		},
		{
			`rate(({app=~"foo|bar"} |~".+bar")[1m])`, time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, factor(5, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "bar"}},
					Points: []promql.Point{{T: 60 * 1000, V: 0.2}, {T: 90 * 1000, V: 0.2}, {T: 120 * 1000, V: 0.2}, {T: 150 * 1000, V: 0.2}, {T: 180 * 1000, V: 0.2}},
				},
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "foo"}},
					Points: []promql.Point{{T: 60 * 1000, V: 0.1}, {T: 90 * 1000, V: 0.1}, {T: 120 * 1000, V: 0.1}, {T: 150 * 1000, V: 0.1}, {T: 180 * 1000, V: 0.1}},
				},
			},
		},
		{
			`absent_over_time(({app="foo"} |~".+bar")[1m])`, time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(1, constant(50), `{app="foo"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `absent_over_time({app="foo"}|~".+bar"[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "foo"}},
					Points: []promql.Point{
						{T: 120000, V: 1}, {T: 150000, V: 1}, {T: 180000, V: 1},
					},
				},
			},
		},
		{
			`rate(({app=~"foo|bar"} |~".+bar" | unwrap bar)[1m])`, time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(10, constantValue(2)), `{app="foo"}`),
					newSeries(testSize, factor(5, constantValue(2)), `{app="bar"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app=~"foo|bar"}|~".+bar"|unwrap bar[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "bar"}},
					Points: []promql.Point{{T: 60 * 1000, V: 0.4}, {T: 90 * 1000, V: 0.4}, {T: 120 * 1000, V: 0.4}, {T: 150 * 1000, V: 0.4}, {T: 180 * 1000, V: 0.4}},
				},
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "foo"}},
					Points: []promql.Point{{T: 60 * 1000, V: 0.2}, {T: 90 * 1000, V: 0.2}, {T: 120 * 1000, V: 0.2}, {T: 150 * 1000, V: 0.2}, {T: 180 * 1000, V: 0.2}},
				},
			},
		},
		{
			`topk(2,rate(({app=~"foo|bar"} |~".+bar")[1m]))`, time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, factor(5, identity), `{app="bar"}`), newSeries(testSize, factor(15, identity), `{app="boo"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "bar"}},
					Points: []promql.Point{{T: 60 * 1000, V: 0.2}, {T: 90 * 1000, V: 0.2}, {T: 120 * 1000, V: 0.2}, {T: 150 * 1000, V: 0.2}, {T: 180 * 1000, V: 0.2}},
				},
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "foo"}},
					Points: []promql.Point{{T: 60 * 1000, V: 0.1}, {T: 90 * 1000, V: 0.1}, {T: 120 * 1000, V: 0.1}, {T: 150 * 1000, V: 0.1}, {T: 180 * 1000, V: 0.1}},
				},
			},
		},
		{
			`topk(1,rate(({app=~"foo|bar"} |~".+bar")[1m]))`, time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, factor(5, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "bar"}},
					Points: []promql.Point{{T: 60 * 1000, V: 0.2}, {T: 90 * 1000, V: 0.2}, {T: 120 * 1000, V: 0.2}, {T: 150 * 1000, V: 0.2}, {T: 180 * 1000, V: 0.2}},
				},
			},
		},
		{
			`topk(1,rate(({app=~"foo|bar"} |~".+bar")[1m])) by (app)`, time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(10, identity), `{app="foo"}`),
					newSeries(testSize, factor(5, identity), `{app="fuzz"}`), newSeries(testSize, identity, `{app="buzz"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "buzz"}},
					Points: []promql.Point{{T: 60 * 1000, V: 1}, {T: 90 * 1000, V: 1}, {T: 120 * 1000, V: 1}, {T: 150 * 1000, V: 1}, {T: 180 * 1000, V: 1}},
				},
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "foo"}},
					Points: []promql.Point{{T: 60 * 1000, V: 0.1}, {T: 90 * 1000, V: 0.1}, {T: 120 * 1000, V: 0.1}, {T: 150 * 1000, V: 0.1}, {T: 180 * 1000, V: 0.1}},
				},
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "fuzz"}},
					Points: []promql.Point{{T: 60 * 1000, V: 0.2}, {T: 90 * 1000, V: 0.2}, {T: 120 * 1000, V: 0.2}, {T: 150 * 1000, V: 0.2}, {T: 180 * 1000, V: 0.2}},
				},
			},
		},
		{
			`bottomk(2,rate(({app=~"foo|bar"} |~".+bar")[1m]))`, time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, factor(20, identity), `{app="bar"}`),
					newSeries(testSize, factor(5, identity), `{app="fuzz"}`), newSeries(testSize, identity, `{app="buzz"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "bar"}},
					Points: []promql.Point{{T: 60 * 1000, V: 0.05}, {T: 90 * 1000, V: 0.05}, {T: 120 * 1000, V: 0.05}, {T: 150 * 1000, V: 0.05}, {T: 180 * 1000, V: 0.05}},
				},
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "foo"}},
					Points: []promql.Point{{T: 60 * 1000, V: 0.1}, {T: 90 * 1000, V: 0.1}, {T: 120 * 1000, V: 0.1}, {T: 150 * 1000, V: 0.1}, {T: 180 * 1000, V: 0.1}},
				},
			},
		},
		{
			`bottomk(3,rate(({app=~"foo|bar|fuzz|buzz"} |~".+bar")[1m])) without (app)`, time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(10, identity), `{app="foo"}`),
					newSeries(testSize, factor(20, identity), `{app="bar"}`),
					newSeries(testSize, factor(5, identity), `{app="fuzz"}`),
					newSeries(testSize, identity, `{app="buzz"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app=~"foo|bar|fuzz|buzz"}|~".+bar"[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "bar"}},
					Points: []promql.Point{{T: 60 * 1000, V: 0.05}, {T: 90 * 1000, V: 0.05}, {T: 120 * 1000, V: 0.05}, {T: 150 * 1000, V: 0.05}, {T: 180 * 1000, V: 0.05}},
				},
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "foo"}},
					Points: []promql.Point{{T: 60 * 1000, V: 0.1}, {T: 90 * 1000, V: 0.1}, {T: 120 * 1000, V: 0.1}, {T: 150 * 1000, V: 0.1}, {T: 180 * 1000, V: 0.1}},
				},
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "fuzz"}},
					Points: []promql.Point{{T: 60 * 1000, V: 0.2}, {T: 90 * 1000, V: 0.2}, {T: 120 * 1000, V: 0.2}, {T: 150 * 1000, V: 0.2}, {T: 180 * 1000, V: 0.2}},
				},
			},
		},
		// binops
		{
			`rate({app="foo"}[1m]) or rate({app="bar"}[1m])`,
			time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(5, identity), `{app="foo"}`),
				},
				{
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app="foo"}[1m])`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app="bar"}[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "bar"}},
					Points: []promql.Point{{T: 60 * 1000, V: 0.2}, {T: 90 * 1000, V: 0.2}, {T: 120 * 1000, V: 0.2}, {T: 150 * 1000, V: 0.2}, {T: 180 * 1000, V: 0.2}},
				},
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "foo"}},
					Points: []promql.Point{{T: 60 * 1000, V: 0.2}, {T: 90 * 1000, V: 0.2}, {T: 120 * 1000, V: 0.2}, {T: 150 * 1000, V: 0.2}, {T: 180 * 1000, V: 0.2}},
				},
			},
		},
		{
			`rate({app="foo"}[1m]) or vector(0)`,
			time.Unix(60, 0), time.Unix(180, 0), 20 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{logproto.Series{
					Labels: `{app="foo"}`,
					Samples: []logproto.Sample{
						{Timestamp: time.Unix(55, 0).UnixNano(), Hash: 1, Value: 1.},
						{Timestamp: time.Unix(60, 0).UnixNano(), Hash: 2, Value: 1.},
						{Timestamp: time.Unix(65, 0).UnixNano(), Hash: 3, Value: 1.},
						{Timestamp: time.Unix(70, 0).UnixNano(), Hash: 4, Value: 1.},
						{Timestamp: time.Unix(170, 0).UnixNano(), Hash: 5, Value: 1.},
						{Timestamp: time.Unix(175, 0).UnixNano(), Hash: 6, Value: 1.},
					},
				}},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app="foo"}[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					//vector result
					Metric: labels.Labels(nil),
					Points: []promql.Point{{T: 60000, V: 0}, {T: 80000, V: 0}, {T: 100000, V: 0}, {T: 120000, V: 0}, {T: 140000, V: 0}, {T: 160000, V: 0}, {T: 180000, V: 0}}},
				promql.Series{
					Metric: labels.Labels{labels.Label{Name: "app", Value: "foo"}},
					Points: []promql.Point{{T: 60000, V: 0.03333333333333333}, {T: 80000, V: 0.06666666666666667}, {T: 100000, V: 0.06666666666666667}, {T: 120000, V: 0.03333333333333333}, {T: 180000, V: 0.03333333333333333}}},
			},
		},
		{
			`
			rate({app=~"foo|bar"}[1m]) and
			rate({app="bar"}[1m])
			`,
			time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(5, identity), `{app="foo"}`),
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
				{
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app=~"foo|bar"}[1m])`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app="bar"}[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "bar"}},
					Points: []promql.Point{{T: 60 * 1000, V: 0.2}, {T: 90 * 1000, V: 0.2}, {T: 120 * 1000, V: 0.2}, {T: 150 * 1000, V: 0.2}, {T: 180 * 1000, V: 0.2}},
				},
			},
		},
		{
			`
			rate({app=~"foo|bar"}[1m]) unless
			rate({app="bar"}[1m])
			`,
			time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(5, identity), `{app="foo"}`),
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
				{
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app=~"foo|bar"}[1m])`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app="bar"}[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "foo"}},
					Points: []promql.Point{{T: 60 * 1000, V: 0.2}, {T: 90 * 1000, V: 0.2}, {T: 120 * 1000, V: 0.2}, {T: 150 * 1000, V: 0.2}, {T: 180 * 1000, V: 0.2}},
				},
			},
		},
		{
			`
			rate({app=~"foo|bar"}[1m]) +
			rate({app="bar"}[1m])
			`,
			time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(5, identity), `{app="foo"}`),
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
				{
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app=~"foo|bar"}[1m])`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app="bar"}[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "bar"}},
					Points: []promql.Point{{T: 60 * 1000, V: 0.4}, {T: 90 * 1000, V: 0.4}, {T: 120 * 1000, V: 0.4}, {T: 150 * 1000, V: 0.4}, {T: 180 * 1000, V: 0.4}},
				},
			},
		},
		{
			`
			rate({app=~"foo|bar"}[1m]) -
			rate({app="bar"}[1m])
			`,
			time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(5, identity), `{app="foo"}`),
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
				{
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app=~"foo|bar"}[1m])`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app="bar"}[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "bar"}},
					Points: []promql.Point{{T: 60 * 1000, V: 0}, {T: 90 * 1000, V: 0}, {T: 120 * 1000, V: 0}, {T: 150 * 1000, V: 0}, {T: 180 * 1000, V: 0}},
				},
			},
		},
		{
			`
			count_over_time({app=~"foo|bar"}[1m]) *
			count_over_time({app="bar"}[1m])
			`,
			time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(5, identity), `{app="foo"}`),
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
				{
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `count_over_time({app=~"foo|bar"}[1m])`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `count_over_time({app="bar"}[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "bar"}},
					Points: []promql.Point{{T: 60 * 1000, V: 144}, {T: 90 * 1000, V: 144}, {T: 120 * 1000, V: 144}, {T: 150 * 1000, V: 144}, {T: 180 * 1000, V: 144}},
				},
			},
		},
		{
			`
			count_over_time({app=~"foo|bar"}[1m]) *
			count_over_time({app="bar"}[1m])
			`,
			time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(5, identity), `{app="foo"}`),
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
				{
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `count_over_time({app=~"foo|bar"}[1m])`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `count_over_time({app="bar"}[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "bar"}},
					Points: []promql.Point{{T: 60 * 1000, V: 144}, {T: 90 * 1000, V: 144}, {T: 120 * 1000, V: 144}, {T: 150 * 1000, V: 144}, {T: 180 * 1000, V: 144}},
				},
			},
		},
		{
			`
			count_over_time({app=~"foo|bar"}[1m]) /
			count_over_time({app="bar"}[1m])
			`,
			time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(5, identity), `{app="foo"}`),
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
				{
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `count_over_time({app=~"foo|bar"}[1m])`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `count_over_time({app="bar"}[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "bar"}},
					Points: []promql.Point{{T: 60 * 1000, V: 1}, {T: 90 * 1000, V: 1}, {T: 120 * 1000, V: 1}, {T: 150 * 1000, V: 1}, {T: 180 * 1000, V: 1}},
				},
			},
		},
		{
			`
			count_over_time({app=~"foo|bar"}[1m]) %
			count_over_time({app="bar"}[1m])
			`,
			time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(5, identity), `{app="foo"}`),
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
				{
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `count_over_time({app=~"foo|bar"}[1m])`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `count_over_time({app="bar"}[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "bar"}},
					Points: []promql.Point{{T: 60 * 1000, V: 0}, {T: 90 * 1000, V: 0}, {T: 120 * 1000, V: 0}, {T: 150 * 1000, V: 0}, {T: 180 * 1000, V: 0}},
				},
			},
		},
		// tests precedence: should be x + (x/x)
		{
			`
			sum by (app) (rate({app=~"foo|bar"} |~".+bar" [1m])) +
			sum by (app) (rate({app=~"foo|bar"} |~".+bar" [1m])) /
			sum by (app) (rate({app=~"foo|bar"} |~".+bar" [1m]))
			`,
			time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(5, identity), `{app="foo"}`),
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `sum by (app) (rate({app=~"foo|bar"} |~".+bar" [1m]))`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "bar"}},
					Points: []promql.Point{{T: 60 * 1000, V: 1.2}, {T: 90 * 1000, V: 1.2}, {T: 120 * 1000, V: 1.2}, {T: 150 * 1000, V: 1.2}, {T: 180 * 1000, V: 1.2}},
				},
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "foo"}},
					Points: []promql.Point{{T: 60 * 1000, V: 1.2}, {T: 90 * 1000, V: 1.2}, {T: 120 * 1000, V: 1.2}, {T: 150 * 1000, V: 1.2}, {T: 180 * 1000, V: 1.2}},
				},
			},
		},
		{
			`avg by (app) (
				sum by (app) (rate({app=~"foo|bar"} |~".+bar" [1m])) +
				sum by (app) (rate({app=~"foo|bar"} |~".+bar" [1m])) /
				sum by (app) (rate({app=~"foo|bar"} |~".+bar" [1m]))
				) * 2
			`,
			time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(5, identity), `{app="foo"}`),
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `sum by (app) (rate({app=~"foo|bar"} |~".+bar" [1m]))`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "bar"}},
					Points: []promql.Point{{T: 60 * 1000, V: 2.4}, {T: 90 * 1000, V: 2.4}, {T: 120 * 1000, V: 2.4}, {T: 150 * 1000, V: 2.4}, {T: 180 * 1000, V: 2.4}},
				},
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "foo"}},
					Points: []promql.Point{{T: 60 * 1000, V: 2.4}, {T: 90 * 1000, V: 2.4}, {T: 120 * 1000, V: 2.4}, {T: 150 * 1000, V: 2.4}, {T: 180 * 1000, V: 2.4}},
				},
			},
		},
		{
			`label_replace(
				avg by (app) (
					sum by (app) (rate({app=~"foo|bar"} |~".+bar" [1m])) +
					sum by (app) (rate({app=~"foo|bar"} |~".+bar" [1m])) /
					sum by (app) (rate({app=~"foo|bar"} |~".+bar" [1m]))
					) * 2,
				"new",
				"$1",
				"app",
				"f(.*)"
			)
			`,
			time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(5, identity), `{app="foo"}`),
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `sum by (app) (rate({app=~"foo|bar"} |~".+bar" [1m]))`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "bar"}},
					Points: []promql.Point{{T: 60 * 1000, V: 2.4}, {T: 90 * 1000, V: 2.4}, {T: 120 * 1000, V: 2.4}, {T: 150 * 1000, V: 2.4}, {T: 180 * 1000, V: 2.4}},
				},
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "foo"}, {Name: "new", Value: "oo"}},
					Points: []promql.Point{{T: 60 * 1000, V: 2.4}, {T: 90 * 1000, V: 2.4}, {T: 120 * 1000, V: 2.4}, {T: 150 * 1000, V: 2.4}, {T: 180 * 1000, V: 2.4}},
				},
			},
		},
		{
			` sum (
					sum by (app) (rate({app=~"foo|bar"} |~".+bar" [1m])) +
					sum by (app) (rate({app=~"foo|bar"} |~".+bar" [1m])) /
					sum by (app) (rate({app=~"foo|bar"} |~".+bar" [1m]))
			) + 1
		`,
			time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(5, identity), `{app="foo"}`),
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `sum by (app) (rate({app=~"foo|bar"} |~".+bar" [1m]))`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{},
					Points: []promql.Point{{T: 60 * 1000, V: 3.4}, {T: 90 * 1000, V: 3.4}, {T: 120 * 1000, V: 3.4}, {T: 150 * 1000, V: 3.4}, {T: 180 * 1000, V: 3.4}},
				},
			},
		},
		{
			`1+1--1`,
			time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			nil,
			nil,
			promql.Matrix{
				promql.Series{
					Points: []promql.Point{{T: 60000, V: 3}, {T: 90000, V: 3}, {T: 120000, V: 3}, {T: 150000, V: 3}, {T: 180000, V: 3}},
				},
			},
		},
		{
			`rate({app="bar"}[1m]) - 1`,
			time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app="bar"}[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "bar"}},
					Points: []promql.Point{{T: 60 * 1000, V: -0.8}, {T: 90 * 1000, V: -0.8}, {T: 120 * 1000, V: -0.8}, {T: 150 * 1000, V: -0.8}, {T: 180 * 1000, V: -0.8}},
				},
			},
		},
		{
			`1 - rate({app="bar"}[1m])`,
			time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app="bar"}[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "bar"}},
					Points: []promql.Point{{T: 60 * 1000, V: 0.8}, {T: 90 * 1000, V: 0.8}, {T: 120 * 1000, V: 0.8}, {T: 150 * 1000, V: 0.8}, {T: 180 * 1000, V: 0.8}},
				},
			},
		},
		{
			`rate({app="bar"}[1m]) - 1 / 2`,
			time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app="bar"}[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "bar"}},
					Points: []promql.Point{{T: 60 * 1000, V: -0.3}, {T: 90 * 1000, V: -0.3}, {T: 120 * 1000, V: -0.3}, {T: 150 * 1000, V: -0.3}, {T: 180 * 1000, V: -0.3}},
				},
			},
		},
		{
			`count_over_time({app="bar"}[1m]) ^ count_over_time({app="bar"}[1m])`,
			time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `count_over_time({app="bar"}[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "bar"}},
					Points: []promql.Point{{T: 60 * 1000, V: math.Pow(12, 12)}, {T: 90 * 1000, V: math.Pow(12, 12)}, {T: 120 * 1000, V: math.Pow(12, 12)}, {T: 150 * 1000, V: math.Pow(12, 12)}, {T: 180 * 1000, V: math.Pow(12, 12)}},
				},
			},
		},
		{
			`2`,
			time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			nil,
			nil,
			promql.Matrix{
				promql.Series{
					Points: []promql.Point{{T: 60 * 1000, V: 2}, {T: 90 * 1000, V: 2}, {T: 120 * 1000, V: 2}, {T: 150 * 1000, V: 2}, {T: 180 * 1000, V: 2}},
				},
			},
		},
		// vector query range
		{
			`vector(2)`,
			time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			nil,
			nil,
			promql.Matrix{
				promql.Series{
					Points: []promql.Point{{T: 60 * 1000, V: 2}, {T: 90 * 1000, V: 2}, {T: 120 * 1000, V: 2}, {T: 150 * 1000, V: 2}, {T: 180 * 1000, V: 2}},
				},
			},
		},
		{
			`bytes_rate({app="foo"}[30s])`, time.Unix(60, 0), time.Unix(120, 0), 15 * time.Second, 0, logproto.FORWARD, 10,
			[][]logproto.Series{
				{logproto.Series{
					Labels: `{app="foo"}`,
					Samples: []logproto.Sample{
						{Timestamp: time.Unix(45, 0).UnixNano(), Hash: 1, Value: 10.}, // 10 bytes / 30s for the first point.
						{Timestamp: time.Unix(60, 0).UnixNano(), Hash: 2, Value: 0.},
						{Timestamp: time.Unix(75, 0).UnixNano(), Hash: 3, Value: 0.},
						{Timestamp: time.Unix(90, 0).UnixNano(), Hash: 4, Value: 0.},
						{Timestamp: time.Unix(105, 0).UnixNano(), Hash: 5, Value: 0.},
					},
				}},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(30, 0), End: time.Unix(120, 0), Selector: `bytes_rate({app="foo"}[30s])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "foo"}},
					Points: []promql.Point{{T: 60 * 1000, V: 10. / 30.}, {T: 75 * 1000, V: 0}, {T: 90 * 1000, V: 0}, {T: 105 * 1000, V: 0}, {T: 120 * 1000, V: 0}},
				},
			},
		},
		{
			`bytes_over_time({app="foo"}[30s])`, time.Unix(60, 0), time.Unix(120, 0), 15 * time.Second, 0, logproto.FORWARD, 10,
			[][]logproto.Series{
				{logproto.Series{
					Labels: `{app="foo"}`,
					Samples: []logproto.Sample{
						{Timestamp: time.Unix(45, 0).UnixNano(), Hash: 1, Value: 5.}, // 5 bytes
						{Timestamp: time.Unix(60, 0).UnixNano(), Hash: 2, Value: 0.},
						{Timestamp: time.Unix(75, 0).UnixNano(), Hash: 3, Value: 0.},
						{Timestamp: time.Unix(90, 0).UnixNano(), Hash: 4, Value: 0.},
						{Timestamp: time.Unix(105, 0).UnixNano(), Hash: 5, Value: 4.}, // 4 bytes
					},
				}},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(30, 0), End: time.Unix(120, 0), Selector: `bytes_over_time({app="foo"}[30s])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "foo"}},
					Points: []promql.Point{{T: 60 * 1000, V: 5.}, {T: 75 * 1000, V: 0}, {T: 90 * 1000, V: 0}, {T: 105 * 1000, V: 4.}, {T: 120 * 1000, V: 4.}},
				},
			},
		},
		{
			`bytes_over_time({app="foo"}[30s]) > bool 1`, time.Unix(60, 0), time.Unix(120, 0), 15 * time.Second, 0, logproto.FORWARD, 10,
			[][]logproto.Series{
				{logproto.Series{
					Labels: `{app="foo"}`,
					Samples: []logproto.Sample{
						{Timestamp: time.Unix(45, 0).UnixNano(), Hash: 1, Value: 5.}, // 5 bytes
						{Timestamp: time.Unix(60, 0).UnixNano(), Hash: 2, Value: 0.},
						{Timestamp: time.Unix(75, 0).UnixNano(), Hash: 3, Value: 0.},
						{Timestamp: time.Unix(90, 0).UnixNano(), Hash: 4, Value: 0.},
						{Timestamp: time.Unix(105, 0).UnixNano(), Hash: 5, Value: 4.}, // 4 bytes
					},
				}},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(30, 0), End: time.Unix(120, 0), Selector: `bytes_over_time({app="foo"}[30s])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "foo"}},
					Points: []promql.Point{{T: 60 * 1000, V: 1.}, {T: 75 * 1000, V: 0}, {T: 90 * 1000, V: 0}, {T: 105 * 1000, V: 1.}, {T: 120 * 1000, V: 1.}},
				},
			},
		},
		{
			`bytes_over_time({app="foo"}[30s]) > 1`, time.Unix(60, 0), time.Unix(120, 0), 15 * time.Second, 0, logproto.FORWARD, 10,
			[][]logproto.Series{
				{logproto.Series{
					Labels: `{app="foo"}`,
					Samples: []logproto.Sample{
						{Timestamp: time.Unix(45, 0).UnixNano(), Hash: 1, Value: 5.}, // 5 bytes
						{Timestamp: time.Unix(60, 0).UnixNano(), Hash: 2, Value: 0.},
						{Timestamp: time.Unix(75, 0).UnixNano(), Hash: 3, Value: 0.},
						{Timestamp: time.Unix(90, 0).UnixNano(), Hash: 4, Value: 0.},
						{Timestamp: time.Unix(105, 0).UnixNano(), Hash: 5, Value: 4.}, // 4 bytes
					},
				}},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(30, 0), End: time.Unix(120, 0), Selector: `bytes_over_time({app="foo"}[30s])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{{Name: "app", Value: "foo"}},
					Points: []promql.Point{{T: 60 * 1000, V: 5.}, {T: 105 * 1000, V: 4.}, {T: 120 * 1000, V: 4.}},
				},
			},
		},
		{
			`bytes_over_time({app="foo"}[30s]) > bool 1`, time.Unix(60, 0), time.Unix(120, 0), 15 * time.Second, 0, logproto.FORWARD, 10,
			[][]logproto.Series{
				{logproto.Series{
					Labels: `{app="foo"}`,
					Samples: []logproto.Sample{
						{Timestamp: time.Unix(45, 0).UnixNano(), Hash: 1, Value: 5.}, // 5 bytes
						{Timestamp: time.Unix(60, 0).UnixNano(), Hash: 2, Value: 0.},
						{Timestamp: time.Unix(75, 0).UnixNano(), Hash: 3, Value: 0.},
						{Timestamp: time.Unix(90, 0).UnixNano(), Hash: 4, Value: 0.},
						{Timestamp: time.Unix(105, 0).UnixNano(), Hash: 5, Value: 4.}, // 4 bytes
					},
				}},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(30, 0), End: time.Unix(120, 0), Selector: `bytes_over_time({app="foo"}[30s])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{labels.Label{Name: "app", Value: "foo"}},
					Points: []promql.Point{
						{T: 60000, V: 1},
						{T: 75000, V: 0},
						{T: 90000, V: 0},
						{T: 105000, V: 1},
						{T: 120000, V: 1},
					},
				},
			},
		},
		{
			// tests combining two streams + unwrap
			`sum(rate({job="foo"} | logfmt | bar > 0 | unwrap bazz [30s]))`, time.Unix(60, 0), time.Unix(120, 0), 30 * time.Second, 0, logproto.FORWARD, 10,
			[][]logproto.Series{
				{
					{
						Labels: `{job="foo", bar="1"}`,
						Samples: []logproto.Sample{
							{Timestamp: time.Unix(40, 0).UnixNano(), Hash: 1, Value: 0.},
							{Timestamp: time.Unix(45, 0).UnixNano(), Hash: 1, Value: 10.},
							{Timestamp: time.Unix(60, 0).UnixNano(), Hash: 2, Value: 0.},
							{Timestamp: time.Unix(90, 0).UnixNano(), Hash: 2, Value: 0.},
							{Timestamp: time.Unix(120, 0).UnixNano(), Hash: 2, Value: 0.},
						},
					},
					{
						Labels: `{job="foo", bar="2"}`,
						Samples: []logproto.Sample{
							{Timestamp: time.Unix(40, 0).UnixNano(), Hash: 1, Value: 0.},
							{Timestamp: time.Unix(45, 0).UnixNano(), Hash: 1, Value: 10.},
							{Timestamp: time.Unix(60, 0).UnixNano(), Hash: 2, Value: 0.},
							{Timestamp: time.Unix(90, 0).UnixNano(), Hash: 2, Value: 0.},
							{Timestamp: time.Unix(120, 0).UnixNano(), Hash: 2, Value: 0.},
						},
					},
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(30, 0), End: time.Unix(120, 0), Selector: `sum(rate({job="foo"} | logfmt | bar > 0  | unwrap bazz [30s]))`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.Labels{},
					Points: []promql.Point{
						{T: 60000, V: 20. / 30.},
						{T: 90000, V: 0},
						{T: 120000, V: 0},
					},
				},
			},
		},
	} {
		test := test
		t.Run(fmt.Sprintf("%s %s", test.qs, test.direction), func(t *testing.T) {
			t.Parallel()

			eng := NewEngine(EngineOpts{}, newQuerierRecorder(t, test.data, test.params), NoLimits, log.NewNopLogger())

			q := eng.Query(LiteralParams{
				qs:        test.qs,
				start:     test.start,
				end:       test.end,
				step:      test.step,
				interval:  test.interval,
				direction: test.direction,
				limit:     test.limit,
			})
			res, err := q.Exec(user.InjectOrgID(context.Background(), "fake"))
			if err != nil {
				t.Fatal(err)
			}
			assert.Equal(t, test.expected, res.Data)
		})
	}
}

type statsQuerier struct{}

func (statsQuerier) SelectLogs(ctx context.Context, p SelectLogParams) (iter.EntryIterator, error) {
	st := stats.FromContext(ctx)
	st.AddDecompressedBytes(1)
	return iter.NoopIterator, nil
}

func (statsQuerier) SelectSamples(ctx context.Context, p SelectSampleParams) (iter.SampleIterator, error) {
	st := stats.FromContext(ctx)
	st.AddDecompressedBytes(1)
	return iter.NoopIterator, nil
}

func TestEngine_Stats(t *testing.T) {
	eng := NewEngine(EngineOpts{}, &statsQuerier{}, NoLimits, log.NewNopLogger())

	queueTime := 2 * time.Nanosecond
	q := eng.Query(LiteralParams{
		qs:        `{foo="bar"}`,
		start:     time.Now(),
		end:       time.Now(),
		direction: logproto.BACKWARD,
		limit:     1000,
	})
	ctx := context.WithValue(context.Background(), httpreq.QueryQueueTimeHTTPHeader, queueTime)
	r, err := q.Exec(user.InjectOrgID(ctx, "fake"))
	require.NoError(t, err)
	require.Equal(t, int64(1), r.Statistics.TotalDecompressedBytes())
	require.Equal(t, queueTime.Seconds(), r.Statistics.Summary.QueueTime)
}

type metaQuerier struct{}

func (metaQuerier) SelectLogs(ctx context.Context, p SelectLogParams) (iter.EntryIterator, error) {
	_ = metadata.JoinHeaders(ctx, []*definitions.PrometheusResponseHeader{
		{
			Name:   "Header",
			Values: []string{"value"},
		},
	})
	return iter.NoopIterator, nil
}

func (metaQuerier) SelectSamples(ctx context.Context, p SelectSampleParams) (iter.SampleIterator, error) {
	_ = metadata.JoinHeaders(ctx, []*definitions.PrometheusResponseHeader{
		{Name: "Header", Values: []string{"value"}},
	})
	return iter.NoopIterator, nil
}

func TestEngine_Metadata(t *testing.T) {
	eng := NewEngine(EngineOpts{}, &metaQuerier{}, NoLimits, log.NewNopLogger())

	q := eng.Query(LiteralParams{
		qs:        `{foo="bar"}`,
		start:     time.Now(),
		end:       time.Now(),
		direction: logproto.BACKWARD,
		limit:     1000,
	})

	r, err := q.Exec(user.InjectOrgID(context.Background(), "fake"))
	require.NoError(t, err)
	require.Equal(t, []*definitions.PrometheusResponseHeader{
		{Name: "Header", Values: []string{"value"}},
	}, r.Headers)
}

func TestEngine_LogsInstantQuery_IllegalLogql(t *testing.T) {
	eng := NewEngine(EngineOpts{}, &statsQuerier{}, NoLimits, log.NewNopLogger())

	queueTime := 2 * time.Nanosecond
	illegalVector := `vector(abc)`
	q := eng.Query(LiteralParams{
		qs:        illegalVector,
		start:     time.Now(),
		end:       time.Now(),
		step:      time.Second * 30,
		interval:  time.Second * 30,
		direction: logproto.BACKWARD,
		limit:     1000,
	})
	expectErr := logqlmodel.NewParseError("syntax error: unexpected IDENTIFIER, expecting NUMBER", 1, 8)
	ctx := context.WithValue(context.Background(), httpreq.QueryQueueTimeHTTPHeader, queueTime)
	_, err := q.Exec(user.InjectOrgID(ctx, "fake"))

	require.EqualError(t, err, expectErr.Error())

	qry, ok := q.(*query)
	require.Equal(t, ok, true)
	vectorExpr := syntax.NewVectorExpr(illegalVector)

	_, err = qry.evalSample(ctx, vectorExpr)
	expectEvalSampleErr := logqlmodel.NewParseError("unable to parse vectorExpr as a float: strconv.ParseFloat: parsing \"vector(abc)\": invalid syntax", 0, 0)
	require.EqualError(t, err, expectEvalSampleErr.Error())
}

func TestEngine_LogsInstantQuery_Vector(t *testing.T) {
	eng := NewEngine(EngineOpts{}, &statsQuerier{}, NoLimits, log.NewNopLogger())
	now := time.Now()
	queueTime := 2 * time.Nanosecond
	logqlVector := `vector(5)`
	q := eng.Query(LiteralParams{
		qs:        logqlVector,
		start:     now,
		end:       now,
		step:      0,
		interval:  time.Second * 30,
		direction: logproto.BACKWARD,
		limit:     1000,
	})
	ctx := context.WithValue(context.Background(), httpreq.QueryQueueTimeHTTPHeader, queueTime)
	_, err := q.Exec(user.InjectOrgID(ctx, "fake"))

	require.NoError(t, err)

	qry, ok := q.(*query)
	require.Equal(t, ok, true)
	vectorExpr := syntax.NewVectorExpr("5")

	data, err := qry.evalSample(ctx, vectorExpr)
	require.NoError(t, err)
	result, ok := data.(promql.Vector)
	require.Equal(t, ok, true)
	require.Equal(t, result[0].V, float64(5))
	require.Equal(t, result[0].T, now.UnixNano()/int64(time.Millisecond))
}

type errorIteratorQuerier struct {
	samples []iter.SampleIterator
	entries []iter.EntryIterator
}

func (e errorIteratorQuerier) SelectLogs(ctx context.Context, p SelectLogParams) (iter.EntryIterator, error) {
	return iter.NewSortEntryIterator(e.entries, p.Direction), nil
}

func (e errorIteratorQuerier) SelectSamples(ctx context.Context, p SelectSampleParams) (iter.SampleIterator, error) {
	return iter.NewSortSampleIterator(e.samples), nil
}

func TestStepEvaluator_Error(t *testing.T) {
	tests := []struct {
		name    string
		qs      string
		querier Querier
		err     error
	}{
		{
			"rangeAggEvaluator",
			`count_over_time({app="foo"}[1m])`,
			&errorIteratorQuerier{
				samples: []iter.SampleIterator{
					iter.NewSeriesIterator(newSeries(testSize, identity, `{app="foo"}`)),
					NewErrorSampleIterator(),
				},
			},
			ErrMock,
		},
		{
			"stream",
			`{app="foo"}`,
			&errorIteratorQuerier{
				entries: []iter.EntryIterator{
					iter.NewStreamIterator(newStream(testSize, identity, `{app="foo"}`)),
					NewErrorEntryIterator(),
				},
			},
			ErrMock,
		},
		{
			"binOpStepEvaluator",
			`count_over_time({app="foo"}[1m]) / count_over_time({app="foo"}[1m])`,
			&errorIteratorQuerier{
				samples: []iter.SampleIterator{
					iter.NewSeriesIterator(newSeries(testSize, identity, `{app="foo"}`)),
					NewErrorSampleIterator(),
				},
			},
			ErrMockMultiple,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			eng := NewEngine(EngineOpts{}, tc.querier, NoLimits, log.NewNopLogger())
			q := eng.Query(LiteralParams{
				qs:    tc.qs,
				start: time.Unix(0, 0),
				end:   time.Unix(180, 0),
				step:  1 * time.Second,
				limit: 1,
			})
			_, err := q.Exec(user.InjectOrgID(context.Background(), "fake"))
			require.Equal(t, tc.err, err)
		})
	}
}

func TestEngine_MaxSeries(t *testing.T) {
	eng := NewEngine(EngineOpts{}, getLocalQuerier(100000), &fakeLimits{maxSeries: 1}, log.NewNopLogger())

	for _, test := range []struct {
		qs             string
		direction      logproto.Direction
		expectLimitErr bool
	}{
		{`topk(1,rate(({app=~"foo|bar"})[1m]))`, logproto.FORWARD, true},
		{`{app="foo"}`, logproto.FORWARD, false},
		{`{app="bar"} |= "foo" |~ ".+bar"`, logproto.BACKWARD, false},
		{`rate({app="foo"} |~".+bar" [1m])`, logproto.BACKWARD, true},
		{`rate({app="foo"}[30s])`, logproto.FORWARD, true},
		{`count_over_time({app="foo|bar"} |~".+bar" [1m])`, logproto.BACKWARD, true},
		{`avg(count_over_time({app=~"foo|bar"} |~".+bar" [1m]))`, logproto.FORWARD, false},
	} {
		t.Run(test.qs, func(t *testing.T) {
			q := eng.Query(LiteralParams{
				qs:        test.qs,
				start:     time.Unix(0, 0),
				end:       time.Unix(100000, 0),
				step:      60 * time.Second,
				direction: test.direction,
				limit:     1000,
			})
			_, err := q.Exec(user.InjectOrgID(context.Background(), "fake"))
			if test.expectLimitErr {
				require.NotNil(t, err)
				require.True(t, errors.Is(err, logqlmodel.ErrLimit))
			} else {
				require.Nil(t, err)
			}
		})
	}
}

func TestEngine_MaxRangeInterval(t *testing.T) {
	eng := NewEngine(EngineOpts{}, getLocalQuerier(100000), &fakeLimits{rangeLimit: 24 * time.Hour, maxSeries: 100000}, log.NewNopLogger())

	for _, test := range []struct {
		qs             string
		direction      logproto.Direction
		expectLimitErr bool
	}{
		{`topk(1,rate(({app=~"foo|bar"})[2d]))`, logproto.FORWARD, true},
		{`topk(1,rate(({app=~"foo|bar"})[1d]))`, logproto.FORWARD, false},
		{`topk(1,rate({app=~"foo|bar"}[12h]) / (rate({app="baz"}[23h]) + rate({app="fiz"}[25h])))`, logproto.FORWARD, true},
	} {
		t.Run(test.qs, func(t *testing.T) {
			q := eng.Query(LiteralParams{
				qs:        test.qs,
				start:     time.Unix(0, 0),
				end:       time.Unix(100000, 0),
				step:      60 * time.Second,
				direction: test.direction,
				limit:     1000,
			})
			_, err := q.Exec(user.InjectOrgID(context.Background(), "fake"))
			if test.expectLimitErr {
				require.Error(t, err)
				require.ErrorIs(t, err, logqlmodel.ErrIntervalLimit)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// go test -mod=vendor ./pkg/logql/ -bench=.  -benchmem -memprofile memprofile.out -cpuprofile cpuprofile.out
func BenchmarkRangeQuery100000(b *testing.B) {
	benchmarkRangeQuery(int64(100000), b)
}

func BenchmarkRangeQuery200000(b *testing.B) {
	benchmarkRangeQuery(int64(200000), b)
}

func BenchmarkRangeQuery500000(b *testing.B) {
	benchmarkRangeQuery(int64(500000), b)
}

func BenchmarkRangeQuery1000000(b *testing.B) {
	benchmarkRangeQuery(int64(1000000), b)
}

var result promql_parser.Value

func benchmarkRangeQuery(testsize int64, b *testing.B) {
	b.ReportAllocs()
	eng := NewEngine(EngineOpts{}, getLocalQuerier(testsize), NoLimits, log.NewNopLogger())
	start := time.Unix(0, 0)
	end := time.Unix(testsize, 0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, test := range []struct {
			qs        string
			direction logproto.Direction
		}{
			{`{app="foo"}`, logproto.FORWARD},
			{`{app="bar"} |= "foo" |~ ".+bar"`, logproto.BACKWARD},
			{`rate({app="foo"} |~".+bar" [1m])`, logproto.BACKWARD},
			{`rate({app="foo"}[30s])`, logproto.FORWARD},
			{`count_over_time({app="foo"} |~".+bar" [1m])`, logproto.BACKWARD},
			{`count_over_time(({app="foo"} |~".+bar")[5m])`, logproto.BACKWARD},
			{`avg(count_over_time({app=~"foo|bar"} |~".+bar" [1m]))`, logproto.FORWARD},
			{`min(rate({app=~"foo|bar"} |~".+bar" [1m]))`, logproto.FORWARD},
			{`max by (app) (rate({app=~"foo|bar"} |~".+bar" [1m]))`, logproto.FORWARD},
			{`max(rate({app=~"foo|bar"} |~".+bar" [1m]))`, logproto.FORWARD},
			{`sum(rate({app=~"foo|bar"} |~".+bar" [1m]))`, logproto.FORWARD},
			{`sum(count_over_time({app=~"foo|bar"} |~".+bar" [1m])) by (app)`, logproto.FORWARD},
			{`count(count_over_time({app=~"foo|bar"} |~".+bar" [1m])) without (app)`, logproto.FORWARD},
			{`stdvar without (app) (count_over_time(({app=~"foo|bar"} |~".+bar")[1m])) `, logproto.FORWARD},
			{`stddev(count_over_time(({app=~"foo|bar"} |~".+bar")[1m])) `, logproto.FORWARD},
			{`rate(({app=~"foo|bar"} |~".+bar")[1m])`, logproto.FORWARD},
			{`topk(2,rate(({app=~"foo|bar"} |~".+bar")[1m]))`, logproto.FORWARD},
			{`topk(1,rate(({app=~"foo|bar"} |~".+bar")[1m]))`, logproto.FORWARD},
			{`topk(1,rate(({app=~"foo|bar"} |~".+bar")[1m])) by (app)`, logproto.FORWARD},
			{`bottomk(2,rate(({app=~"foo|bar"} |~".+bar")[1m]))`, logproto.FORWARD},
			{`bottomk(3,rate(({app=~"foo|bar"} |~".+bar")[1m])) without (app)`, logproto.FORWARD},
		} {
			q := eng.Query(LiteralParams{
				qs:        test.qs,
				start:     start,
				end:       end,
				step:      60 * time.Second,
				direction: test.direction,
				limit:     1000,
			})
			res, err := q.Exec(user.InjectOrgID(context.Background(), "fake"))
			if err != nil {
				b.Fatal(err)
			}
			result = res.Data
			if result == nil {
				b.Fatal("unexpected nil result")
			}
		}
	}
}

// TestHashingStability tests logging stability between engine and RecordRangeAndInstantQueryMetrics methods.
func TestHashingStability(t *testing.T) {
	ctx := user.InjectOrgID(context.Background(), "fake")
	params := LiteralParams{
		start:     time.Unix(0, 0),
		end:       time.Unix(5, 0),
		step:      60 * time.Second,
		direction: logproto.FORWARD,
		limit:     1000,
	}

	queryWithEngine := func() string {
		buf := bytes.NewBufferString("")
		logger := log.NewLogfmtLogger(buf)
		eng := NewEngine(EngineOpts{LogExecutingQuery: true}, getLocalQuerier(4), NoLimits, logger)
		query := eng.Query(params)
		_, err := query.Exec(ctx)
		require.NoError(t, err)
		return buf.String()
	}

	queryDirectly := func() string {
		statsResult := stats.Result{
			Summary: stats.Summary{
				BytesProcessedPerSecond: 100000,
				QueueTime:               0.000000002,
				ExecTime:                25.25,
				TotalBytesProcessed:     100000,
				TotalEntriesReturned:    10,
			},
		}
		buf := bytes.NewBufferString("")
		logger := log.NewLogfmtLogger(buf)
		RecordRangeAndInstantQueryMetrics(ctx, logger, params, "200", statsResult, logqlmodel.Streams{logproto.Stream{Entries: make([]logproto.Entry, 10)}})
		return buf.String()
	}

	for _, test := range []struct {
		qs string
	}{
		{`sum by(query_hash) (count_over_time({app="myapp",env="myenv"} |= "error" |= "metrics.go" | logfmt [10s]))`},
		{`sum (count_over_time({app="myapp",env="myenv"} |= "error" |= "metrics.go" | logfmt [10s])) by(query_hash)`},
	} {
		params.qs = test.qs
		expectedQueryHash := HashedQuery(test.qs)

		// check that both places will end up having the same query hash, even though they're emitting different log lines.
		require.Regexp(t,
			regexp.MustCompile(
				fmt.Sprintf(
					`level=info org_id=fake msg="executing query" type=range query=.* length=5s step=1m0s query_hash=%d.*`, expectedQueryHash,
				),
			),
			queryWithEngine(),
		)

		require.Regexp(t,
			regexp.MustCompile(
				fmt.Sprintf(
					`level=info org_id=fake latency=slow query=".*" query_hash=%d query_type=metric range_type=range.*\n`, expectedQueryHash,
				),
			),
			queryDirectly(),
		)
	}
}

func getLocalQuerier(size int64) Querier {
	return &querierRecorder{
		series: map[string][]logproto.Series{
			"": {
				newSeries(size, identity, `{app="foo"}`),
				newSeries(size, identity, `{app="foo",bar="foo"}`),
				newSeries(size, identity, `{app="foo",bar="bazz"}`),
				newSeries(size, identity, `{app="foo",bar="fuzz"}`),
				newSeries(size, identity, `{app="bar"}`),
				newSeries(size, identity, `{app="bar",bar="foo"}`),
				newSeries(size, identity, `{app="bar",bar="bazz"}`),
				newSeries(size, identity, `{app="bar",bar="fuzz"}`),
			},
		},
		streams: map[string][]logproto.Stream{
			"": {
				newStream(size, identity, `{app="foo"}`),
				newStream(size, identity, `{app="foo",bar="foo"}`),
				newStream(size, identity, `{app="foo",bar="bazz"}`),
				newStream(size, identity, `{app="foo",bar="fuzz"}`),
				newStream(size, identity, `{app="bar"}`),
				newStream(size, identity, `{app="bar",bar="foo"}`),
				newStream(size, identity, `{app="bar",bar="bazz"}`),
				newStream(size, identity, `{app="bar",bar="fuzz"}`),
			},
		},
	}
}

type querierRecorder struct {
	streams map[string][]logproto.Stream
	series  map[string][]logproto.Series
	match   bool
}

func newQuerierRecorder(t *testing.T, data interface{}, params interface{}) *querierRecorder {
	t.Helper()
	streams := map[string][]logproto.Stream{}
	if streamsIn, ok := data.([][]logproto.Stream); ok {
		if paramsIn, ok2 := params.([]SelectLogParams); ok2 {
			for i, p := range paramsIn {
				streams[paramsID(p)] = streamsIn[i]
			}
		}
	}

	series := map[string][]logproto.Series{}
	if seriesIn, ok := data.([][]logproto.Series); ok {
		if paramsIn, ok2 := params.([]SelectSampleParams); ok2 {
			for i, p := range paramsIn {
				series[paramsID(p)] = seriesIn[i]
			}
		}
	}
	return &querierRecorder{
		streams: streams,
		series:  series,
		match:   true,
	}
}

func (q *querierRecorder) SelectLogs(ctx context.Context, p SelectLogParams) (iter.EntryIterator, error) {
	if !q.match {
		for _, s := range q.streams {
			return iter.NewStreamsIterator(s, p.Direction), nil
		}
	}
	recordID := paramsID(p)
	streams, ok := q.streams[recordID]
	if !ok {
		return nil, fmt.Errorf("no streams found for id: %s has: %+v", recordID, q.streams)
	}
	return iter.NewStreamsIterator(streams, p.Direction), nil
}

func (q *querierRecorder) SelectSamples(ctx context.Context, p SelectSampleParams) (iter.SampleIterator, error) {
	if !q.match {
		for _, s := range q.series {
			return iter.NewMultiSeriesIterator(s), nil
		}
	}
	recordID := paramsID(p)
	if len(q.series) == 0 {
		return iter.NoopIterator, nil
	}
	series, ok := q.series[recordID]
	if !ok {
		return nil, fmt.Errorf("no series found for id: %s has: %+v", recordID, q.series)
	}
	return iter.NewMultiSeriesIterator(series), nil
}

func paramsID(p interface{}) string {
	b, err := json.Marshal(p)
	if err != nil {
		panic(err)
	}
	return strings.ReplaceAll(string(b), " ", "")
}

type logData struct {
	logproto.Entry
	// nolint
	logproto.Sample
}

type generator func(i int64) logData

func newStream(n int64, f generator, lbsString string) logproto.Stream {
	labels, err := syntax.ParseLabels(lbsString)
	if err != nil {
		panic(err)
	}
	entries := []logproto.Entry{}
	for i := int64(0); i < n; i++ {
		entries = append(entries, f(i).Entry)
	}
	return logproto.Stream{
		Entries: entries,
		Labels:  labels.String(),
	}
}

func newSeries(n int64, f generator, lbsString string) logproto.Series {
	labels, err := syntax.ParseLabels(lbsString)
	if err != nil {
		panic(err)
	}
	samples := []logproto.Sample{}
	for i := int64(0); i < n; i++ {
		samples = append(samples, f(i).Sample)
	}
	return logproto.Series{
		Samples: samples,
		Labels:  labels.String(),
	}
}

func newIntervalStream(n int64, step time.Duration, f generator, labels string) logproto.Stream {
	entries := []logproto.Entry{}
	lastEntry := int64(-100) // Start with a really small value (negative) so we always output the first item
	for i := int64(0); int64(len(entries)) < n; i++ {
		if float64(lastEntry)+step.Seconds() <= float64(i) {
			entries = append(entries, f(i).Entry)
			lastEntry = i
		}
	}
	return logproto.Stream{
		Entries: entries,
		Labels:  labels,
	}
}

func newBackwardStream(n int64, f generator, labels string) logproto.Stream {
	entries := []logproto.Entry{}
	for i := n - 1; i > 0; i-- {
		entries = append(entries, f(i).Entry)
	}
	return logproto.Stream{
		Entries: entries,
		Labels:  labels,
	}
}

func newBackwardIntervalStream(n, expectedResults int64, step time.Duration, f generator, labels string) logproto.Stream {
	entries := []logproto.Entry{}
	lastEntry := int64(100000) // Start with some really big value so that we always output the first item
	for i := n - 1; int64(len(entries)) < expectedResults; i-- {
		if float64(lastEntry)-step.Seconds() >= float64(i) {
			entries = append(entries, f(i).Entry)
			lastEntry = i
		}
	}
	return logproto.Stream{
		Entries: entries,
		Labels:  labels,
	}
}

func identity(i int64) logData {
	return logData{
		Entry: logproto.Entry{
			Timestamp: time.Unix(i, 0),
			Line:      fmt.Sprintf("%d", i),
		},
		Sample: logproto.Sample{
			Timestamp: time.Unix(i, 0).UnixNano(),
			Value:     1.,
			Hash:      uint64(i),
		},
	}
}

// nolint
func factor(j int64, g generator) generator {
	return func(i int64) logData {
		return g(i * j)
	}
}

// nolint
func offset(j int64, g generator) generator {
	return func(i int64) logData {
		return g(i + j)
	}
}

// nolint
func constant(t int64) generator {
	return func(i int64) logData {
		return logData{
			Entry: logproto.Entry{
				Timestamp: time.Unix(t, 0),
				Line:      fmt.Sprintf("%d", i),
			},
			Sample: logproto.Sample{
				Timestamp: time.Unix(t, 0).UnixNano(),
				Hash:      uint64(i),
				Value:     1.0,
			},
		}
	}
}

// nolint
func constantValue(t int64) generator {
	return func(i int64) logData {
		return logData{
			Entry: logproto.Entry{
				Timestamp: time.Unix(i, 0),
				Line:      fmt.Sprintf("%d", i),
			},
			Sample: logproto.Sample{
				Timestamp: time.Unix(i, 0).UnixNano(),
				Hash:      uint64(i),
				Value:     float64(t),
			},
		}
	}
}

// nolint
func incValue(val int64) generator {
	return func(i int64) logData {
		return logData{
			Entry: logproto.Entry{
				Timestamp: time.Unix(i, 0),
				Line:      fmt.Sprintf("%d", i),
			},
			Sample: logproto.Sample{
				Timestamp: time.Unix(i, 0).UnixNano(),
				Hash:      uint64(i),
				Value:     float64(val + i),
			},
		}
	}
}

// nolint
func inverse(g generator) generator {
	return func(i int64) logData {
		return g(-i)
	}
}

// errorIterator
type errorIterator struct{}

// NewErrorSampleIterator return an sample iterator that errors out
func NewErrorSampleIterator() iter.SampleIterator {
	return &errorIterator{}
}

// NewErrorEntryIterator return an entry iterator that errors out
func NewErrorEntryIterator() iter.EntryIterator {
	return &errorIterator{}
}

func (errorIterator) Next() bool { return false }

func (errorIterator) Error() error { return ErrMock }

func (errorIterator) Labels() string { return "" }

func (errorIterator) StreamHash() uint64 { return 0 }

func (errorIterator) Entry() logproto.Entry { return logproto.Entry{} }

func (errorIterator) Sample() logproto.Sample { return logproto.Sample{} }

func (errorIterator) Close() error { return nil }
