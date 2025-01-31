// SPDX-FileCopyrightText: 2014-2022 SAP SE
//
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"fmt"
)

// JWT implements JWT authentication.
type JWT struct {
	token     string
	logonname string
	_cookie   []byte
}

// NewJWT creates a new authJWT instance.
func NewJWT(token string) *JWT { return &JWT{token: token} }

func (a *JWT) String() string { return fmt.Sprintf("method type %s token %s", a.Typ(), a.token) }

// SetToken implements the AuthTokenSetter interface.
func (a *JWT) SetToken(token string) { a.token = token }

// Cookie implements the CookieGetter interface.
func (a *JWT) Cookie() (string, []byte) { return a.logonname, a._cookie }

// Typ implements the CookieGetter interface.
func (a *JWT) Typ() string { return MtJWT }

// Order implements the CookieGetter interface.
func (a *JWT) Order() byte { return MoJWT }

// PrepareInitReq implements the Method interface.
func (a *JWT) PrepareInitReq(prms *Prms) {
	prms.addString(a.Typ())
	prms.addString(a.token)
}

// InitRepDecode implements the Method interface.
func (a *JWT) InitRepDecode(d *Decoder) error {
	a.logonname = d.String()
	Tracef("JWT auth - logonname: %v", a.logonname)
	return nil
}

// PrepareFinalReq implements the Method interface.
func (a *JWT) PrepareFinalReq(prms *Prms) error {
	prms.AddCESU8String(a.logonname)
	prms.addString(a.Typ())
	prms.addEmpty() // empty parameter
	return nil
}

// FinalRepDecode implements the Method interface.
func (a *JWT) FinalRepDecode(d *Decoder) error {
	if err := d.NumPrm(2); err != nil {
		return err
	}
	mt := d.String()
	if err := checkAuthMethodType(mt, a.Typ()); err != nil {
		return err
	}
	a._cookie = d.bytes()
	Tracef("JWT auth - cookie: %v", a._cookie)
	return nil
}
