/*
Copyright 2019 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package metrics

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	network "knative.dev/networking/pkg"
	"net/http"
)

const (
	JSONContentType      = "application/json"
	PlainTextContentType = "text/plain"
)

type customHTTPScrapeClient struct {
	httpClient *http.Client
}

func newCustomHTTPScrapeClient(httpClient *http.Client) *customHTTPScrapeClient {
	return &customHTTPScrapeClient{
		httpClient: httpClient,
	}
}

func (c *customHTTPScrapeClient) Do(req *http.Request) (CustomStat, error) {
	req.Header.Add("Accept", network.ProtoAcceptContent)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return emptyCustomStat, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return emptyCustomStat, scrapeError{
			error:       fmt.Errorf("GET request for URL %q returned HTTP status %v", req.URL.String(), resp.StatusCode),
			mightBeMesh: network.IsPotentialMeshErrorResponse(resp),
		}
	}
	if resp.Header.Get("Content-Type") != JSONContentType && resp.Header.Get("Content-Type") != PlainTextContentType {
		return emptyCustomStat, fmt.Errorf("unsupported Content-Type: %s", resp.Header.Get("Content-Type"))
	}
	return customStatFromJSON(resp.Body)
}

func customStatFromJSON(body io.Reader) (CustomStat, error) {
	var stat CustomStat
	b := pool.Get().(*bytes.Buffer)
	b.Reset()
	defer pool.Put(b)
	_, err := b.ReadFrom(body)
	if err != nil {
		return emptyCustomStat, fmt.Errorf("reading body failed: %w", err)
	}
	err = json.Unmarshal(b.Bytes(), &stat)
	if err != nil {
		return emptyCustomStat, fmt.Errorf("unmarshalling failed: %w (%s)", err, string(b.Bytes()))
	}
	return stat, nil
}
