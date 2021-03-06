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
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/ioutil"
	"net/http"
	"strings"

	"github.com/Comcast/webpa-common/device"
	"github.com/Comcast/webpa-common/wrp"
	"github.com/go-ozzo/ozzo-validation"
)

//Vars shortens frequently used type returned by mux.Vars()
type Vars map[string]string

var (
	//SET errors
	errEmptyNames     = errors.New("names parameter is required to be valid")
	errInvalidSetWDMP = errors.New("invalid XPC SET message")
	errNewCIDRequired = errors.New("NewCid is required for TEST_AND_SET")

	//PUT errors
	errTableNameRequired = errors.New("could not get parameter var from http.Request")
)

//ConversionTool lays out the definition of methods to build WDMP from content in an http request
type ConversionTool interface {
	GetFlavorFormat(*http.Request, Vars, string, string, string) (*GetWDMP, error)
	SetFlavorFormat(*http.Request) (*SetWDMP, error)
	DeleteFlavorFormat(Vars, string) (*DeleteRowWDMP, error)
	AddFlavorFormat(io.Reader, Vars, string) (*AddRowWDMP, error)
	ReplaceFlavorFormat(io.Reader, Vars, string) (*ReplaceRowsWDMP, error)

	ValidateAndDeduceSET(http.Header, *SetWDMP) error
	GetFromURLPath(string, Vars) (string, bool)
	GetConfiguredWRP([]byte, Vars, http.Header) *wrp.Message
}

//EncodingHelper implements the definitions defined in EncodingTool
type EncodingHelper struct{}

//ConversionWDMP implements the definitions defined in ConversionTool
type ConversionWDMP struct {
	WRPSource string
}

//The following functions with names of the form {command}FlavorFormat serve as the low level builders of WDMP objects
// based on the commands they serve for Cloud <-> TR181 devices communication

//GetFlavorFormat constructs a WDMP object out of the contents of a given request. Supports the GET command
func (cw *ConversionWDMP) GetFlavorFormat(req *http.Request, pathVars Vars, attr, namesKey, sep string) (wdmp *GetWDMP, err error) {
	wdmp = new(GetWDMP)

	if nameGroup := req.FormValue(namesKey); nameGroup != "" {
		wdmp.Command = CommandGet
		wdmp.Names = strings.Split(nameGroup, sep)
	} else {
		err = errEmptyNames
		return
	}

	if attributes := req.FormValue(attr); attributes != "" {
		wdmp.Command = CommandGetAttrs
		wdmp.Attribute = attributes
	}

	return
}

//SetFlavorFormat has analogous functionality to GetFlavorformat but instead supports the various SET commands
func (cw *ConversionWDMP) SetFlavorFormat(req *http.Request) (wdmp *SetWDMP, err error) {
	wdmp = new(SetWDMP)

	var payload []byte
	if payload, err = ioutil.ReadAll(req.Body); err == nil {
		if err = json.Unmarshal(payload, wdmp); err == nil || len(payload) == 0 {
			err = cw.ValidateAndDeduceSET(req.Header, wdmp)
		}
	}

	return
}

//DeleteFlavorFormat again has analogous functionality to GetFlavormat but for the DELETE_ROW command
func (cw *ConversionWDMP) DeleteFlavorFormat(urlVars Vars, rowKey string) (wdmp *DeleteRowWDMP, err error) {
	wdmp = &DeleteRowWDMP{Command: CommandDeleteRow}

	if row, exists := cw.GetFromURLPath(rowKey, urlVars); exists && row != "" {
		wdmp.Row = row
	} else {
		err = errors.New("non-empty row name is required")
		return
	}
	return
}

//AddFlavorFormat supports the ADD_ROW command
func (cw *ConversionWDMP) AddFlavorFormat(input io.Reader, urlVars Vars, tableName string) (wdmp *AddRowWDMP, err error) {
	wdmp = &AddRowWDMP{Command: CommandAddRow}

	if table, exists := cw.GetFromURLPath(tableName, urlVars); exists {
		wdmp.Table = table
	} else {
		err = errTableNameRequired
		return
	}

	var payload []byte
	if payload, err = ioutil.ReadAll(input); err == nil && len(payload) > 0 {
		if err = json.Unmarshal(payload, &wdmp.Row); err == nil {
			err = validation.Validate(wdmp.Row, validation.NotNil)
		}
	}
	return
}

//ReplaceFlavorFormat supports the REPLACE_ROWS command
func (cw *ConversionWDMP) ReplaceFlavorFormat(input io.Reader, urlVars Vars, tableName string) (wdmp *ReplaceRowsWDMP, err error) {
	wdmp = &ReplaceRowsWDMP{Command: CommandReplaceRows}

	if table, exists := cw.GetFromURLPath(tableName, urlVars); exists {
		wdmp.Table = table
	} else {
		err = errTableNameRequired
		return
	}

	var payload []byte
	if payload, err = ioutil.ReadAll(input); err == nil && len(payload) > 0 {
		if err = json.Unmarshal(payload, &wdmp.Rows); err == nil {
			err = validation.Validate(wdmp.Rows, validation.NotNil)
		}
	}

	return
}

//ValidateAndDeduceSET deduces the command for a given wdmp object and validates it for such
func (cw *ConversionWDMP) ValidateAndDeduceSET(header http.Header, wdmp *SetWDMP) (err error) {
	newCID, oldCID, syncCMC := header.Get(HeaderWPASyncNewCID), header.Get(HeaderWPASyncOldCID), header.Get(HeaderWPASyncCMC)

	if newCID == "" && oldCID != "" {
		err = errNewCIDRequired
		return
	} else if newCID == "" && oldCID == "" && syncCMC == "" {
		wdmp.Command = getCommandForParam(wdmp.Parameters)
	} else {
		wdmp.Command = CommandTestSet
		wdmp.NewCid = newCID
		wdmp.OldCid = oldCID
		wdmp.SyncCmc = syncCMC
	}

	if !isValidSetWDMP(wdmp) {
		err = errInvalidSetWDMP
	}

	return
}

//getCommandForParams decides whether the command for some request is a 'SET' or 'SET_ATTRS' based on a given list of parameters
func getCommandForParam(params []SetParam) (command string) {
	command = CommandSet
	if params == nil || len(params) < 1 {
		return
	}
	if wdmp := params[0]; wdmp.Attributes != nil &&
		wdmp.Name != nil &&
		wdmp.DataType == nil &&
		wdmp.Value == nil {
		command = CommandSetAttrs
	}
	return
}

//validate servers as a helper function to determine whether the given Set WDMP object is valid for its context
func isValidSetWDMP(wdmp *SetWDMP) (isValid bool) {
	if emptyParams := wdmp.Parameters == nil || len(wdmp.Parameters) == 0; emptyParams {
		return wdmp.Command == CommandTestSet //TEST_AND_SET can have empty parameters
	}

	cmdSetAttr := 0
	cmdSet := 0

	//validate parameters if it exists, even for TEST_SET
	for _, param := range wdmp.Parameters {
		if param.Name == nil || *param.Name == "" {
			return
		}

		if param.Value != nil && (param.DataType == nil || *param.DataType < 0) {
			return
		}

		if wdmp.Command == CommandSetAttrs && param.Attributes == nil {
			return
		}

		if param.Attributes != nil &&
			param.DataType == nil &&
			param.Value == nil {

			cmdSetAttr++
		} else {
			cmdSet++
		}

		// verify that all parameters are correct for either doing a command SET_ATTRIBUTE or SET
		if cmdSetAttr > 0 && cmdSet > 0 {
			return
		}
	}
	return true
}

//GetFromURLPath Same as invoking urlVars[key] directly but urlVars can be nil in which case key does not exist in it
func (cw *ConversionWDMP) GetFromURLPath(key string, urlVars Vars) (val string, exists bool) {
	if urlVars != nil {
		val, exists = urlVars[key]
	}
	return
}

//GetConfiguredWRP Set the necessary fields in the wrp and return it
func (cw *ConversionWDMP) GetConfiguredWRP(wdmp []byte, pathVars Vars, header http.Header) (wrpMsg *wrp.Message) {
	deviceID, _ := cw.GetFromURLPath("deviceid", pathVars)
	canonicalDeviceID, _ := device.ParseID(deviceID)
	service, _ := cw.GetFromURLPath("service", pathVars)

	wrpMsg = &wrp.Message{
		Type:            wrp.SimpleRequestResponseMessageType,
		ContentType:     header.Get("Content-Type"),
		Payload:         wdmp,
		Source:          cw.GetWRPSource() + "/" + service,
		Destination:     string(canonicalDeviceID) + "/" + service,
		TransactionUUID: GetOrGenTID(header),
	}
	return
}

// GetWRPSource returns the Source that should be used in every
// WRP transaction message
func (cw *ConversionWDMP) GetWRPSource() string {
	return cw.WRPSource
}

//GetOrGenTID returns a Transaction ID for a given request.
//If a TID was provided in the headers, such is used. Otherwise,
//a new TID is generated and returned
func GetOrGenTID(requestHeader http.Header) (tid string) {
	if tid = requestHeader.Get(HeaderWPATID); tid == "" {
		buf := make([]byte, 16)
		if _, err := rand.Read(buf); err == nil {
			tid = base64.RawURLEncoding.EncodeToString(buf)
		}
	}
	return
}
