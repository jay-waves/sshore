package jsonstream

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"io"
)

type Encoder struct{ *jsontext.Encoder }
type Decoder struct{ *jsontext.Decoder }

func NewEncoder(w io.Writer) *Encoder { return &Encoder{jsontext.NewEncoder(w)} }
func NewDecoder(r io.Reader) *Decoder { return &Decoder{jsontext.NewDecoder(r)} }

func (e *Encoder) Encode(value any) error { return json.MarshalEncode(e.Encoder, value) }
func (d *Decoder) Decode(value any) error { return json.UnmarshalDecode(d.Decoder, value) }
