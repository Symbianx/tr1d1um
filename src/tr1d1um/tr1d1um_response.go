/**
 * Copyright 2017 Comcast Cable Communications Management, LLC
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
 *
 */
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Comcast/webpa-common/wrp"
)

//Tr1StatusTimeout is a custom status representing multiple webpa-specific timeout cases (context, http.client, service unavailable)
const Tr1StatusTimeout = 503

var errUnexpectedRKDResponse = errors.New("unexpectedRDKResponse")

//RDKResponse will serve as the struct to read in
//results coming from some RDK device
type RDKResponse struct {
	StatusCode int `json:"statusCode"`
}

//Tr1d1umResponse serves as a container for http responses to Tr1d1um
//Due to the nature of retries, only the final container's data gets
//transferred to an http.ResponseWriter
type Tr1d1umResponse struct {
	Body    []byte
	Code    int
	Headers http.Header
	err     error
}

//New helps initialize values that avoid nil exceptions and keeps
//consistent status code standard with an http.ResponseWriter
func (tr1Resp Tr1d1umResponse) New() *Tr1d1umResponse {
	tr1Resp.Headers, tr1Resp.Body, tr1Resp.Code = http.Header{}, []byte{}, http.StatusOK
	return &tr1Resp
}

//WriteResponse is a tiny helper function that passes responses (In Json format only for now)
//to a caller
func WriteResponse(message string, statusCode int, tr1Resp *Tr1d1umResponse) {
	tr1Resp.Headers.Set("Content-Type", wrp.JSON.ContentType())
	tr1Resp.Code = statusCode
	tr1Resp.Body = []byte(fmt.Sprintf(`{"message":"%s"}`, message))
}

//WriteResponseWriter is a tiny helper function that passes responses (In Json format only for now)
//to a caller through a ResponseWriter
func WriteResponseWriter(message string, statusCode int, origin http.ResponseWriter) {
	origin.Header().Set("Content-Type", wrp.JSON.ContentType())
	origin.WriteHeader(statusCode)
	origin.Write([]byte(fmt.Sprintf(`{"message":"%s"}`, message)))
}

//ReportError checks (given that the given error is not nil) if the error is related to a timeout. If it is, it marks it as so.
//Else, it defaults to an InternalError
func ReportError(err error, tr1Resp *Tr1d1umResponse) {
	if err == nil {
		return
	}
	message, statusCode := "", http.StatusInternalServerError

	if errMsg := err.Error(); strings.HasSuffix(errMsg, "context canceled") ||
		strings.HasSuffix(errMsg, "deadline exceeded") ||
		strings.Contains(errMsg, "Client.Timeout exceeded") {
		message, statusCode = "Error Timeout", Tr1StatusTimeout
	}

	WriteResponse(message, statusCode, tr1Resp)

	return
}

//GetStatusCodeFromRDKResponse returns the status code given a well-formatted
//RDK response. Otherwise, it defaults to 500 as code and returns a pertinent error
func GetStatusCodeFromRDKResponse(RDKPayload []byte) (statusCode int, err error) {
	RDKResp := new(RDKResponse)
	statusCode = http.StatusInternalServerError
	if err = json.Unmarshal(RDKPayload, RDKResp); err == nil {
		if RDKResp.StatusCode != 0 { // some statusCode was actually provided
			statusCode = RDKResp.StatusCode
		} else {
			err = errUnexpectedRKDResponse
		}
	}
	return
}
