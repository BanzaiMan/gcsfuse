// Copyright 2015 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package httputil

import (
	"io"
	"net/http"
	"net/url"
)

// Create an HTTP request with the supplied information.
//
// Unlike http.NewRequest:
//
//  *  This function doesn't mangle the supplied URL by round tripping it to a
//     string. For example, the Opaque field will continue to differentiate
//     between actual slashes in the path and escaped ones (cf.
//     http://goo.gl/rWX6ps).
//
//  *  This function doesn't magically re-interpret an io.Reader as an
//     io.ReadCloser when possible.
//
//  *  This function provides a convenient choke point to ensure we don't
//     forget to set a user agent header.
//
func NewRequest(
	method string,
	url *url.URL,
	body io.ReadCloser,
	userAgent string) (req *http.Request, err error) {
	// Create the request.
	req = &http.Request{
		Method:     method,
		URL:        url,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     make(http.Header),
		Body:       body,
		Host:       url.Host,
	}

	// Set the User-Agent header.
	req.Header.Set("User-Agent", userAgent)

	return
}
