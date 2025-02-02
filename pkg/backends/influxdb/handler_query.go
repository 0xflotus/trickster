/*
 * Copyright 2018 The Trickster Authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package influxdb

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/trickstercache/trickster/v2/pkg/proxy/engines"
	"github.com/trickstercache/trickster/v2/pkg/proxy/errors"
	"github.com/trickstercache/trickster/v2/pkg/proxy/headers"
	"github.com/trickstercache/trickster/v2/pkg/proxy/params"
	"github.com/trickstercache/trickster/v2/pkg/proxy/urls"
	"github.com/trickstercache/trickster/v2/pkg/timeseries"

	"github.com/influxdata/influxql"
)

// QueryHandler handles timeseries requests for InfluxDB and processes them through the delta proxy cache
func (c *Client) QueryHandler(w http.ResponseWriter, r *http.Request) {

	qp, _, _ := params.GetRequestValues(r)
	q := strings.Trim(strings.ToLower(qp.Get(upQuery)), " \t\n")
	if q == "" {
		c.ProxyHandler(w, r)
		return
	}

	// if it's not a select statement, just proxy it instead
	if strings.Index(q, "select ") == -1 {
		c.ProxyHandler(w, r)
		return
	}

	r.URL = urls.BuildUpstreamURL(r, c.BaseUpstreamURL())
	engines.DeltaProxyCacheRequest(w, r, c.Modeler())
}

var epochToFlag = map[string]byte{
	"ns": 1,
	"u":  2, "µ": 2,
	"ms": 3,
	"s":  4,
	"m":  5,
	"h":  6,
}

// ParseTimeRangeQuery parses the key parts of a TimeRangeQuery from the inbound HTTP Request
func (c *Client) ParseTimeRangeQuery(r *http.Request) (*timeseries.TimeRangeQuery,
	*timeseries.RequestOptions, bool, error) {

	trq := &timeseries.TimeRangeQuery{Extent: timeseries.Extent{}}
	rlo := &timeseries.RequestOptions{}

	var valuer = &influxql.NowValuer{Now: time.Now()}

	v, _, _ := params.GetRequestValues(r)
	if trq.Statement = v.Get(upQuery); trq.Statement == "" {
		return nil, nil, false, errors.MissingURLParam(upQuery)
	}

	if b, ok := epochToFlag[v.Get(upEpoch)]; ok {
		rlo.TimeFormat = b
	}

	if v.Get(upPretty) == "true" {
		rlo.OutputFormat = 1
	} else if r != nil && r.Header != nil &&
		r.Header.Get(headers.NameAccept) == headers.ValueApplicationCSV {
		rlo.OutputFormat = 2
	}

	p := influxql.NewParser(strings.NewReader(trq.Statement))
	q, err := p.ParseQuery()
	if err != nil {
		return nil, nil, false, err
	}

	trq.Step = -1
	var hasTimeQueryParts bool
	statements := make([]string, 0, len(q.Statements))
	var cacheError error
	for _, v := range q.Statements {
		sel, ok := v.(*influxql.SelectStatement)
		if !ok || sel.Condition == nil {
			cacheError = errors.ErrNotTimeRangeQuery
		}
		step, err := sel.GroupByInterval()
		if err != nil {
			cacheError = err
		} else {
			if trq.Step == -1 && step > 0 {
				trq.Step = step
			} else if trq.Step != step {
				// this condition means multiple queries were present, and had
				// different step widths
				cacheError = errors.ErrStepParse
			}
		}
		_, tr, err := influxql.ConditionExpr(sel.Condition, valuer)
		if err != nil {
			cacheError = err
		}

		// this section determines the time range of the query
		ex := timeseries.Extent{Start: tr.Min, End: tr.Max}
		if ex.Start.IsZero() {
			ex.Start = time.Unix(0, 0)
		}
		if ex.End.IsZero() {
			ex.End = time.Now()
		}
		if trq.Extent.Start.IsZero() {
			trq.Extent = ex
		} else if trq.Extent != ex {
			// this condition means multiple queries were present, and had
			// different time ranges
			cacheError = errors.ErrNotTimeRangeQuery
		}

		// this sets a zero time range for normalizing the query for cache key hashing
		sel.SetTimeRange(time.Time{}, time.Time{})
		statements = append(statements, sel.String())

		hasTimeQueryParts = true
	}

	if !hasTimeQueryParts {
		cacheError = errors.ErrNotTimeRangeQuery
	}

	// this field is used as part of the data that calculates the cache key
	trq.Statement = strings.Join(statements, " ; ")
	trq.ParsedQuery = q
	trq.TemplateURL = urls.Clone(r.URL)
	qt := url.Values(http.Header(v).Clone())
	qt.Set(upQuery, trq.Statement)

	// Swap in the Tokenzed Query in the Url Params
	trq.TemplateURL.RawQuery = qt.Encode()

	if cacheError != nil {
		return trq, rlo, true, cacheError
	}

	return trq, rlo, false, nil

}
