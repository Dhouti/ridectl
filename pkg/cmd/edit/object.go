/*
Copyright 2019 Ridecell, Inc.

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

package edit

import (
	"encoding/base64"
	"io"
	"regexp"

	secretsv1beta1 "github.com/Ridecell/ridecell-operator/pkg/apis/secrets/v1beta1"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/kms"
	"github.com/aws/aws-sdk-go/service/kms/kmsiface"
	"github.com/pkg/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"

	hacksecretsv1beta1 "github.com/Ridecell/ridectl/pkg/apis/secrets/v1beta1"
)

var dataRegexp *regexp.Regexp
var keyRegexp *regexp.Regexp

func init() {
	dataRegexp = regexp.MustCompile(`(?ms)kind: (EncryptedSecret|DecryptedSecret).*?(^data:.*?)\z`)
	keyRegexp = regexp.MustCompile(`(?m)^[ \t]+([^:\n\r]+):[ \t]*(.+?)[ \t]*$`)
}

func NewObject(raw []byte) (*Object, error) {
	o := &Object{Raw: raw}
	// First do the normal parse to see if we have any errors.
	// Ignored return value is a GVK.
	obj, _, err := scheme.Codecs.UniversalDeserializer().Decode(raw, nil, nil)
	if err != nil {
		return nil, errors.Wrap(err, "error parsing YAML")
	}
	o.Object = obj
	o.Meta = obj.(metav1.Object)

	// Check if this an EncryptedSecret.
	enc, ok := obj.(*secretsv1beta1.EncryptedSecret)
	if ok {
		o.OrigEnc = enc
		o.Kind = "EncryptedSecret"
		o.Data = enc.Data
	}
	// or a DecryptedSecret.
	dec, ok2 := obj.(*hacksecretsv1beta1.DecryptedSecret)
	if ok2 {
		o.AfterDec = dec
		o.Kind = "DecryptedSecret"
		o.Data = dec.Data
	}

	if ok || ok2 {
		// Run the regex parse. If you are reading this code, I am sorry and yes I
		// feel bad about it. This is used when re-encoding to allow output that
		// preserves comments, whitespace, key ordering, etc.
		match := dataRegexp.FindSubmatchIndex(raw)
		if match == nil {
			// This shouldn't happen.
			panic("EncryptedSecret or DecryptedSecret didn't match dataRegexp")
		}
		// match[0] and [1] are for the whole regexp, we don't need that.
		o.KindLoc.Start = match[2]
		o.KindLoc.End = match[3]
		o.DataLoc.Start = match[4]
		o.DataLoc.End = match[5]
		locs, err := newKeysLocations(raw[o.DataLoc.Start:o.DataLoc.End], o.DataLoc.Start)
		if err != nil {
			// Also shouldn't happen.
			panic(err.Error())
		}
		o.KeyLocs = locs

		// A safety check for now.
		if len(o.Data) != len(o.KeyLocs) {
			panic("key count mismatch")
		}
	}
	return o, nil
}

func newKeysLocations(raw []byte, offset int) ([]KeysLocation, error) {
	matches := keyRegexp.FindAllSubmatchIndex(raw, -1)
	if matches == nil {
		return nil, errors.New("unable to parse keys")
	}
	locs := []KeysLocation{}
	for _, match := range matches {
		keyStart := match[2]
		keyEnd := match[3]
		valueLoc := TextLocation{Start: match[4] + offset, End: match[5] + offset}
		key := string(raw[keyStart:keyEnd])
		if key[0] == '#' {
			// Go doesn't do negative lookaheads to easier to filter comments out here.
			continue
		}
		locs = append(locs, KeysLocation{TextLocation: valueLoc, Key: key})
	}
	return locs, nil
}

func (o *Object) Decrypt(kmsService kmsiface.KMSAPI) error {
	if o.Kind == "" {
		return nil
	}

	dec := &DecryptedSecret{ObjectMeta: o.OrigEnc.ObjectMeta, Data: map[string]string{}}
	for key, value := range o.OrigEnc.Data {
		decodedValue := make([]byte, base64.StdEncoding.DecodedLen(len(value)))
		l, err := base64.StdEncoding.Decode(decodedValue, []byte(value))
		if err != nil {
			return errors.Wrapf(err, "error base64 decoding value for %s", key)
		}
		decryptedValue, err := kmsService.Decrypt(&kms.DecryptInput{
			CiphertextBlob: decodedValue[:l],
			EncryptionContext: map[string]*string{
				"RidecellOperator": aws.String("true"),
			},
		})
		if err != nil {
			return errors.Wrapf(err, "error decrypting value for %s", key)
		}
		// Check if values in this secret were encrypted with more than one key.
		if o.KeyId != "" && o.KeyId != *decryptedValue.KeyId {
			return errors.Errorf("key mismatch between %s and %s for %s", o.KeyId, *decryptedValue.KeyId, key)
		}
		o.KeyId = *decryptedValue.KeyId
		dec.Data[key] = string(decryptedValue.Plaintext)
	}
	o.OrigDec = dec
	o.Kind = "DecryptedSecret"
	o.Data = dec.Data
	return nil
}

func (o *Object) Encrypt(kmsService kmsiface.KMSAPI, defaultKeyId string) error {
	if o.Kind == "" {
		return nil
	}

	// Work out which key to use.
	keyId := defaultKeyId
	if o.KeyId != "" {
		keyId = o.KeyId
	}

	enc := &secretsv1beta1.EncryptedSecret{ObjectMeta: o.AfterDec.ObjectMeta, Data: map[string]string{}}
	for key, value := range o.AfterDec.Data {
		// Check if this key has changed.
		if o.OrigDec != nil && o.OrigEnc != nil {
			origDecValue, ok := o.OrigDec.Data[key]
			if ok && value == origDecValue {
				// Key was not changed, reuse the old encrypted value.
				enc.Data[key] = o.OrigEnc.Data[key]
				continue
			}
		}
		// Encrypt the new value.
		encryptedValue, err := kmsService.Encrypt(&kms.EncryptInput{
			KeyId:     aws.String(keyId),
			Plaintext: []byte(value),
			// This encryption context is used for access control policies.
			EncryptionContext: map[string]*string{
				"RidecellOperator": aws.String("true"),
			},
		})
		if err != nil {
			return errors.Wrapf(err, "error encrypting value for %s", key)
		}
		enc.Data[key] = base64.StdEncoding.EncodeToString(encryptedValue.CiphertextBlob)
	}
	o.AfterEnc = enc
	o.Kind = "EncryptedSecret"
	o.Data = enc.Data
	return nil
}

func (o *Object) Serialize(out io.Writer) error {
	// Check if this is one of the two types we care about.
	if o.Data == nil {
		// Nope, we're out.
		_, err := out.Write(o.Raw)
		return err
	}

	// Start writing!
	_, err := out.Write(o.Raw[0:o.KindLoc.Start])
	if err != nil {
		return err
	}
	_, err = out.Write([]byte(o.Kind))
	if err != nil {
		return err
	}
	// Track where we are up to.
	carry := o.KindLoc.End
	for _, keyLoc := range o.KeyLocs {
		newValue, ok := o.Data[keyLoc.Key]
		if !ok {
			panic("key from location not found in data")
		}
		_, err = out.Write(o.Raw[carry:keyLoc.Start])
		if err != nil {
			return err
		}
		_, err = out.Write([]byte(newValue))
		if err != nil {
			return err
		}
		carry = keyLoc.End
	}
	_, err = out.Write(o.Raw[carry:])
	if err != nil {
		return err
	}

	return nil
}
