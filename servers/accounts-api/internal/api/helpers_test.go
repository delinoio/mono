// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"net/http"

	"github.com/delinoio/mono/servers/accounts-api/internal/directory"
)

func directoryUser(primaryEmail string) directory.User {
	return directory.User{PrimaryEmail: primaryEmail}
}

func newPreflight(url, origin string) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodOptions, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Origin", origin)
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	req.Header.Set("Access-Control-Request-Headers", "authorization,content-type")
	return req, nil
}

func httpDo(req *http.Request) (*http.Response, error) {
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	_ = resp.Body.Close()
	return resp, nil
}
