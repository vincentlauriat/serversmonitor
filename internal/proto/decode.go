package proto

import (
	"encoding/json"
	"errors"
	"reflect"
)

var ErrUnknownType = errors.New("proto: unknown message type")

type envelope struct {
	V    int    `json:"v"`
	Type string `json:"type"`
}

// Decode parses one message. Unknown fields are ignored on purpose: a newer
// agent may send metrics this hub does not know yet.
func Decode(data []byte) (any, error) {
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, err
	}
	var msg any
	switch env.Type {
	case TypeHello:
		msg = &Hello{}
	case TypeWelcome:
		msg = &Welcome{}
	case TypeSample:
		msg = &Sample{}
	case TypeReconfigure:
		msg = &Reconfigure{}
	default:
		return nil, ErrUnknownType
	}
	if err := json.Unmarshal(data, msg); err != nil {
		return nil, err
	}
	return msg, nil
}

// Encode marshals a message after stamping the protocol version.
// msg must be a pointer to one of the message structs with Type set.
func Encode(msg any) ([]byte, error) {
	v := reflect.ValueOf(msg)
	if v.Kind() != reflect.Pointer || v.Elem().Kind() != reflect.Struct {
		return nil, errors.New("proto: Encode wants a pointer to a message struct")
	}
	e := v.Elem()
	if e.FieldByName("Type").String() == "" {
		return nil, errors.New("proto: message has no type")
	}
	e.FieldByName("V").SetInt(Version)
	return json.Marshal(msg)
}
