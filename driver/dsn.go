// SPDX-FileCopyrightText: 2014-2022 SAP SE
//
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// DSN parameters. For parameter client locale see http://help.sap.com/hana/SAP_HANA_SQL_Command_Network_Protocol_Reference_en.pdf.
const (
	DSNDefaultSchema = "defaultSchema" // Database default schema.
	DSNTimeout       = "timeout"       // Driver side connection timeout in seconds.
	DSNPingInterval  = "pingInterval"  // Connection ping interval in seconds.
)

/*
DSN TLS parameters.
For more information please see https://golang.org/pkg/crypto/tls/#Config.
For more flexibility in TLS configuration please see driver.Connector.
*/
const (
	DSNTLSRootCAFile         = "TLSRootCAFile"         // Path,- filename to root certificate(s).
	DSNTLSServerName         = "TLSServerName"         // ServerName to verify the hostname.
	DSNTLSInsecureSkipVerify = "TLSInsecureSkipVerify" // Controls whether a client verifies the server's certificate chain and host name.
)

// TLSPrms is holding the TLS parameters of a DSN structure.
type TLSPrms struct {
	ServerName         string
	InsecureSkipVerify bool
	RootCAFiles        []string
}

const urlSchema = "hdb" // mirrored from driver/DriverName

/*
A DSN represents a parsed DSN string. A DSN string is an URL string with the following format

	"hdb://<username>:<password>@<host address>:<port number>"

and optional query parameters (see DSN query parameters and DSN query default values).

Example:
	"hdb://myuser:mypassword@localhost:30015?timeout=60"

Examples TLS connection:
	"hdb://myuser:mypassword@localhost:39013?TLSRootCAFile=trust.pem"
	"hdb://myuser:mypassword@localhost:39013?TLSRootCAFile=trust.pem&TLSServerName=hostname"
	"hdb://myuser:mypassword@localhost:39013?TLSInsecureSkipVerify"
*/
type DSN struct {
	host               string
	username, password string
	defaultSchema      string
	timeout            time.Duration
	pingInterval       time.Duration
	tls                *TLSPrms
}

// ParseError is the error returned in case DSN is invalid.
type ParseError struct {
	s   string
	err error
}

func (e ParseError) Error() string {
	if err := errors.Unwrap(e.err); err != nil {
		return err.Error()
	}
	return e.s
}

// Unwrap returns the nested error.
func (e ParseError) Unwrap() error { return e.err }

// Cause returns the cause of the error.
func (e ParseError) Cause() error { return e.err }

//
func parameterNotSupportedError(k string) error {
	return &ParseError{s: fmt.Sprintf("parameter %s is not supported", k)}
}
func invalidNumberOfParametersError(k string, act, exp int) error {
	return &ParseError{s: fmt.Sprintf("invalid number of parameters for %s %d - expected %d", k, act, exp)}
}
func invalidNumberOfParametersRangeError(k string, act, min, max int) error {
	return &ParseError{s: fmt.Sprintf("invalid number of parameters for %s %d - expected %d - %d", k, act, min, max)}
}
func invalidNumberOfParametersMinError(k string, act, min int) error {
	return &ParseError{s: fmt.Sprintf("invalid number of parameters for %s %d - expected at least %d", k, act, min)}
}
func parseError(k, v string) error {
	return &ParseError{s: fmt.Sprintf("failed to parse %s: %s", k, v)}
}

// parseDSN parses a DSN string into a DSN structure.
func parseDSN(s string) (*DSN, error) {
	if s == "" {
		return nil, &ParseError{s: "invalid parameter - DSN is empty"}
	}

	u, err := url.Parse(s)
	if err != nil {
		return nil, &ParseError{err: err}
	}

	dsn := &DSN{host: u.Host}
	if u.User != nil {
		dsn.username = u.User.Username()
		password, _ := u.User.Password()
		dsn.password = password
	}

	for k, v := range u.Query() {
		switch k {

		default:
			return nil, parameterNotSupportedError(k)

		case DSNDefaultSchema:
			if len(v) != 1 {
				return nil, invalidNumberOfParametersError(k, len(v), 1)
			}
			dsn.defaultSchema = v[0]

		case DSNTimeout:
			if len(v) != 1 {
				return nil, invalidNumberOfParametersError(k, len(v), 1)
			}
			t, err := strconv.Atoi(v[0])
			if err != nil {
				return nil, parseError(k, v[0])
			}
			dsn.timeout = time.Duration(t) * time.Second

		case DSNPingInterval:
			if len(v) != 1 {
				return nil, invalidNumberOfParametersError(k, len(v), 1)
			}
			t, err := strconv.Atoi(v[0])
			if err != nil {
				return nil, parseError(k, v[0])
			}
			dsn.pingInterval = time.Duration(t) * time.Second

		case DSNTLSServerName:
			if len(v) != 1 {
				return nil, invalidNumberOfParametersError(k, len(v), 1)
			}
			if dsn.tls == nil {
				dsn.tls = &TLSPrms{}
			}
			dsn.tls.ServerName = v[0]

		case DSNTLSInsecureSkipVerify:
			if len(v) > 1 {
				return nil, invalidNumberOfParametersRangeError(k, len(v), 0, 1)
			}
			b := true
			if len(v) > 0 && v[0] != "" {
				b, err = strconv.ParseBool(v[0])
				if err != nil {
					return nil, parseError(k, v[0])
				}
			}
			if dsn.tls == nil {
				dsn.tls = &TLSPrms{}
			}
			dsn.tls.InsecureSkipVerify = b

		case DSNTLSRootCAFile:
			if len(v) == 0 {
				return nil, invalidNumberOfParametersMinError(k, len(v), 1)
			}
			if dsn.tls == nil {
				dsn.tls = &TLSPrms{}
			}
			dsn.tls.RootCAFiles = v
		}
	}
	return dsn, nil
}

// String reassembles the DSN into a valid DSN string.
func (dsn *DSN) String() string {
	values := url.Values{}
	if dsn.defaultSchema != "" {
		values.Set(DSNDefaultSchema, dsn.defaultSchema)
	}
	if dsn.timeout != 0 {
		values.Set(DSNTimeout, fmt.Sprintf("%d", dsn.timeout/time.Second))
	}
	if dsn.pingInterval != 0 {
		values.Set(DSNPingInterval, fmt.Sprintf("%d", dsn.pingInterval/time.Second))
	}
	if dsn.tls != nil {
		if dsn.tls.ServerName != "" {
			values.Set(DSNTLSServerName, dsn.tls.ServerName)
		}
		values.Set(DSNTLSInsecureSkipVerify, strconv.FormatBool(dsn.tls.InsecureSkipVerify))
		for _, fn := range dsn.tls.RootCAFiles {
			values.Add(DSNTLSRootCAFile, fn)
		}
	}
	u := &url.URL{
		Scheme:   urlSchema,
		Host:     dsn.host,
		RawQuery: values.Encode(),
	}
	switch {
	case dsn.username != "" && dsn.password != "":
		u.User = url.UserPassword(dsn.username, dsn.password)
	case dsn.username != "":
		u.User = url.User(dsn.username)
	}
	return u.String()
}
