// SPDX-FileCopyrightText: 2014-2022 SAP SE
//
// SPDX-License-Identifier: Apache-2.0

package auth

// Salted Challenge Response Authentication Mechanism (SCRAM)

import (
	"fmt"
)

// SCRAMSHA256 implements SCRAMSHA256 authentication.
type SCRAMSHA256 struct {
	username, password       string
	clientChallenge          []byte
	salt, serverChallenge    []byte
	clientProof, serverProof []byte
}

// NewSCRAMSHA256 creates a new authSCRAMSHA256 instance.
func NewSCRAMSHA256(username, password string) *SCRAMSHA256 {
	return &SCRAMSHA256{username: username, password: password, clientChallenge: clientChallenge()}
}

func (a *SCRAMSHA256) String() string {
	return fmt.Sprintf("method type %s clientChallenge %v", a.Typ(), a.clientChallenge)
}

// SetPassword implemets the AuthPasswordSetter interface.
func (a *SCRAMSHA256) SetPassword(password string) { a.password = password }

// Typ implements the CookieGetter interface.
func (a *SCRAMSHA256) Typ() string { return MtSCRAMSHA256 }

// Order implements the CookieGetter interface.
func (a *SCRAMSHA256) Order() byte { return MoSCRAMSHA256 }

// PrepareInitReq implements the Method interface.
func (a *SCRAMSHA256) PrepareInitReq(prms *Prms) {
	prms.addString(a.Typ())
	prms.addBytes(a.clientChallenge)
}

// InitRepDecode implements the Method interface.
func (a *SCRAMSHA256) InitRepDecode(d *Decoder) error {
	d.subSize() // sub parameters
	if err := d.NumPrm(2); err != nil {
		return err
	}
	a.salt = d.bytes()
	a.serverChallenge = d.bytes()
	if err := checkSalt(a.salt); err != nil {
		return err
	}
	if err := checkServerChallenge(a.serverChallenge); err != nil {
		return err
	}
	return nil
}

// PrepareFinalReq implements the Method interface.
func (a *SCRAMSHA256) PrepareFinalReq(prms *Prms) error {
	key := scramsha256Key([]byte(a.password), a.salt)
	a.clientProof = clientProof(key, a.salt, a.serverChallenge, a.clientChallenge)
	if err := checkClientProof(a.clientProof); err != nil {
		return err
	}

	prms.AddCESU8String(a.username)
	prms.addString(a.Typ())
	subPrms := prms.addPrms()
	subPrms.addBytes(a.clientProof)

	return nil
}

// FinalRepDecode implements the Method interface.
func (a *SCRAMSHA256) FinalRepDecode(d *Decoder) error {
	if err := d.NumPrm(2); err != nil {
		return err
	}
	mt := d.String()
	if err := checkAuthMethodType(mt, a.Typ()); err != nil {
		return err
	}
	if d.subSize() == 0 { // mnSCRAMSHA256: server does not return server proof parameter
		return nil
	}
	if err := d.NumPrm(1); err != nil {
		return err
	}
	a.serverProof = d.bytes()
	return nil
}

func scramsha256Key(password, salt []byte) []byte {
	return _sha256(_hmac(password, salt))
}
