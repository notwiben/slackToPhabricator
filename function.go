// Copyright 2019 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// [START functions_slack_search]

// Package slack is a Cloud Function which recieves a query from
// a Slack command and responds with the KG API result.
package slack

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/uber/gonduit"
	"github.com/uber/gonduit/core"
	"github.com/uber/gonduit/requests"
)

type oldTimeStampError struct {
	s string
}

func (e *oldTimeStampError) Error() string {
	return e.s
}

const (
	version                     = "v0"
	slackRequestTimestampHeader = "X-Slack-Request-Timestamp"
	slackSignatureHeader        = "X-Slack-Signature"
)

type attachment struct {
	Color     string   `json:"color"`
	Title     string   `json:"title"`
	TitleLink string   `json:"title_link"`
	Text      string   `json:"text"`
	ImageURL  string   `json:"image_url"`
	Fields    []fields `json:"fields"`
}

type fields struct {
	Title string `json:"title"`
	Value string `json:"value"`
	Short bool   `json:"short"`
}

// Message is the a Slack message event.
// see https://api.slack.com/docs/message-formatting
type Message struct {
	ResponseType string       `json:"response_type"`
	Text         string       `json:"text"`
	Attachments  []attachment `json:"attachments"`
}

// F uses the Knowledge Graph API to search for a query provided
// by a Slack command.
func F(w http.ResponseWriter, r *http.Request) {
	setup(r.Context())

	bodyBytes, err := ioutil.ReadAll(r.Body)
	if err != nil {
		log.Fatalf("Couldn't read request body: %v", err)
	}
	r.Body = ioutil.NopCloser(bytes.NewBuffer(bodyBytes))

	if r.Method != "POST" {
		http.Error(w, "Only POST requests are accepted", 405)
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Couldn't parse form", 400)
		log.Fatalf("ParseForm: %v", err)
	}

	// Reset r.Body as ParseForm depletes it by reading the io.ReadCloser.
	r.Body = ioutil.NopCloser(bytes.NewBuffer(bodyBytes))

	result, err := verifyWebHook(r, slackSecret)
	if err != nil {
		log.Fatalf("verifyWebhook: %v", err)
	}
	if !result {
		log.Fatalf("signatures did not match.")
	}

	if len(r.Form["text"]) == 0 {
		log.Fatalf("empty text in form")
	}

	res, err := makeSearchRequest(r.Form["text"][0])
	if err != nil {
		log.Fatalf("makeSearchRequest: %v", err)
	}

	w.Header().Set("Content-Type", "application/json")
	if err = json.NewEncoder(w).Encode(res); err != nil {
		log.Fatalf("json.Marshal: %v", err)
	}
}

func makeSearchRequest(query string) (*Message, error) {

	message := &Message{}

	client, err := gonduit.Dial(
		"https://phabricator.sirclo.com",
		&core.ClientOptions{
			APIToken: phabAPIToken,
		},
	)

	ce, ok := err.(*core.ConduitError)
	if ok {
		log.Fatal("code: " + ce.Code())
		log.Fatal("info: " + ce.Info())
	}

	// Or, use the built-in utility function:
	if core.IsConduitError(err) {
		log.Fatal(err)
		return message, err
	}

	if strings.HasPrefix(query, "T") || strings.HasPrefix(query, "t") {
		message, err = requestManiphestDetail(client, query)
		if err != nil {
			return message, err
		}
	}
	return message, err
}

func requestManiphestDetail(client *gonduit.Conn, query string) (message *Message, err error) {

	maniphestID, err := strconv.Atoi(query[1:])
	if err != nil {
		return message, err
	}

	req := requests.ManiphestSearchRequest{
		Constraints: &requests.ManiphestSearchConstraints{
			IDs: []int{maniphestID},
		},
	}

	res, err := client.ManiphestSearch(req)
	if err != nil {
		return message, err
	}

	if len(res.Data) <= 0 {
		message = &Message{
			ResponseType: "in_channel",
			Text:         "Task not found",
			Attachments: []attachment{
				{
					Text: "Please refine your search",
				},
			},
		}
		return message, nil
	}

	t := time.Time(res.Data[0].Fields.DateCreated)
	created := t.Format("2006-01-02")

	t = time.Time(res.Data[0].Fields.DateModified)
	lastModified := t.Format("2006-01-02")

	message = &Message{
		ResponseType: "in_channel",
		Text:         fmt.Sprintf("https://phabricator.sirclo.com/T%s", query[1:]),
		Attachments: []attachment{
			{
				Title:     fmt.Sprintf("%s", res.Data[0].Fields.Name),
				TitleLink: fmt.Sprintf("https://phabricator.sirclo.com/T%s", query[1:]),
				Fields: []fields{
					{
						Title: "Description",
						Value: res.Data[0].Fields.Description.Raw,
						Short: false,
					},
					{
						Title: "Status",
						Value: res.Data[0].Fields.Status.Value,
						Short: false,
					},
					{
						Title: "Created",
						Value: created,
						Short: false,
					},
					{
						Title: "Last Updated",
						Value: lastModified,
						Short: false,
					},
				},
			},
		},
	}
	return message, nil
}

// verifyWebHook verifies the request signature.
// See https://api.slack.com/docs/verifying-requests-from-slack.
func verifyWebHook(r *http.Request, slackSigningSecret string) (bool, error) {
	timeStamp := r.Header.Get(slackRequestTimestampHeader)
	slackSignature := r.Header.Get(slackSignatureHeader)

	if timeStamp == "" || slackSignature == "" {
		return false, fmt.Errorf("timestamp or slack signature is empty")
	}

	t, err := strconv.ParseInt(timeStamp, 10, 64)
	if err != nil {
		return false, fmt.Errorf("strconv.ParseInt(%s): %v", timeStamp, err)
	}

	if ageOk, age := checkTimestamp(t); !ageOk {
		return false, &oldTimeStampError{fmt.Sprintf("checkTimestamp(%v): %v %v", t, ageOk, age)}
		// return false, fmt.Errorf("checkTimestamp(%v): %v %v", t, ageOk, age)
	}

	if timeStamp == "" || slackSignature == "" {
		return false, fmt.Errorf("either timeStamp or signature headers were blank")
	}

	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return false, fmt.Errorf("ioutil.ReadAll(%v): %v", r.Body, err)
	}

	// Reset the body so other calls won't fail.
	r.Body = ioutil.NopCloser(bytes.NewBuffer(body))

	baseString := fmt.Sprintf("%s:%s:%s", version, timeStamp, body)

	signature := getSignature([]byte(baseString), []byte(slackSigningSecret))

	trimmed := strings.TrimPrefix(slackSignature, fmt.Sprintf("%s=", version))
	signatureInHeader, err := hex.DecodeString(trimmed)

	if err != nil {
		return false, fmt.Errorf("hex.DecodeString(%v): %v", trimmed, err)
	}

	return hmac.Equal(signature, signatureInHeader), nil
}

func getSignature(base []byte, secret []byte) []byte {
	h := hmac.New(sha256.New, secret)
	h.Write(base)

	return h.Sum(nil)
}

// Arbitrarily trusting requests time stamped less than 5 minutes ago.
func checkTimestamp(timeStamp int64) (bool, time.Duration) {
	t := time.Since(time.Unix(timeStamp, 0))

	return t.Minutes() <= 5, t
}
