// Package schema defines the messages Murmur sends over NATS.
//
// v1 is JSON (plan §13, open item 1): easy to read while debugging; Protobuf
// only if Phase 4 shows JSON is the bottleneck.
package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/gomavlib/v3/pkg/dialects/ardupilotmega"
	"github.com/bluenviron/gomavlib/v3/pkg/message"

	"github.com/ChengCK18/murmur/internal/mavlink"
)

// Telemetry is one MAVLink message from a drone, as published on
// telemetry.<drone_id>:
//
//	{
//	  "tenant_id": 1, "drone_id": 42, "sys_id": 1, "comp_id": 1, "seq": 17,
//	  "msg_id": 33, "msg": "GLOBAL_POSITION_INT",
//	  "received_at": "2026-11-01T10:00:00.123456789Z",
//	  "time_boot_ms": 1234567,
//	  "payload": { "lat": -353632620, "lon": 1491652370, "relative_alt": 20100, "hdg": 35300, ... }
//	}
//
// Payload fields use MAVLink's own names and raw values (enums as numbers,
// lat/lon as degrees × 10⁷, and so on), so the MAVLink message reference
// describes them. JSON has no NaN or infinity, so those float values are
// written as null, and null reads back as NaN. MAVLink uses NaN for "unknown"
// (ArduPilot fills HOME_POSITION.q with it).
type Telemetry struct {
	TenantID   int64     `json:"tenant_id"`
	DroneID    int64     `json:"drone_id"`
	SysID      uint8     `json:"sys_id"`
	CompID     uint8     `json:"comp_id"`
	Seq        uint8     `json:"seq"`
	MsgID      uint32    `json:"msg_id"`
	Msg        string    `json:"msg"`
	ReceivedAt time.Time `json:"received_at"`
	// TimeBootMs is the drone's boot clock, for message types that carry one.
	TimeBootMs *uint32         `json:"time_boot_ms,omitempty"`
	Payload    json.RawMessage `json:"payload"`
}

// FromReceived builds the Telemetry for a decoded frame.
func FromReceived(tenantID, droneID int64, r mavlink.Received) (Telemetry, error) {
	if _, ok := r.Msg.(*message.MessageRaw); ok {
		return Telemetry{}, fmt.Errorf("schema: message ID %d is not in the dialect", r.Msg.GetID())
	}
	payload, err := marshalPayload(r.Msg)
	if err != nil {
		return Telemetry{}, err
	}
	t := Telemetry{
		TenantID:   tenantID,
		DroneID:    droneID,
		SysID:      r.SysID,
		CompID:     r.CompID,
		Seq:        r.Seq,
		MsgID:      r.Msg.GetID(),
		Msg:        MessageName(r.Msg),
		ReceivedAt: r.ReceivedAt.UTC(),
		Payload:    payload,
	}
	if ms, ok := mavlink.TimeBootMs(r.Msg); ok {
		t.TimeBootMs = &ms
	}
	return t, nil
}

// Subject is the NATS subject the message is published on.
func (t Telemetry) Subject() string {
	return "telemetry." + strconv.FormatInt(t.DroneID, 10)
}

// DedupID is the Nats-Msg-Id header value. JetStream drops a second message
// with the same ID inside its duplicate window, so a publish retried after a
// timeout is stored once.
func (t Telemetry) DedupID() string {
	return fmt.Sprintf("%d:%s:%d", t.DroneID, t.Msg, t.ReceivedAt.UnixNano())
}

// Message rebuilds the typed MAVLink message from the payload, such as
// *ardupilotmega.MessageGlobalPositionInt.
func (t Telemetry) Message() (message.Message, error) {
	typ, ok := messageTypes()[t.MsgID]
	if !ok {
		return nil, fmt.Errorf("schema: unknown message ID %d", t.MsgID)
	}
	var fields map[string]any
	dec := json.NewDecoder(bytes.NewReader(t.Payload))
	dec.UseNumber()
	if err := dec.Decode(&fields); err != nil {
		return nil, fmt.Errorf("schema: %s payload: %w", t.Msg, err)
	}
	v := reflect.New(typ)
	for _, f := range fieldsOf(typ) {
		raw, ok := fields[f.name]
		if !ok {
			continue // a missing field keeps its zero value
		}
		if err := setValue(v.Elem().Field(f.index), raw, f.bits); err != nil {
			return nil, fmt.Errorf("schema: %s.%s: %w", t.Msg, f.name, err)
		}
	}
	return v.Interface().(message.Message), nil
}

// MessageName returns the MAVLink name of a message, such as
// "GLOBAL_POSITION_INT".
func MessageName(m message.Message) string {
	return goToDef(reflect.TypeOf(m).Elem().Name()[len("Message"):], strings.ToUpper)
}

// goToDef turns a Go name into a MAVLink one, as gomavlib does when computing
// checksums: GlobalPositionInt → GLOBAL_POSITION_INT, RelativeAlt → relative_alt.
func goToDef(name string, toCase func(string) string) string {
	return toCase(capital.ReplaceAllString(name, "_${1}")[1:])
}

var capital = regexp.MustCompile("([A-Z])")

type field struct {
	index int
	name  string
	bits  int // width on the wire, for range checks: an enum is a uint64 in Go but often a uint8 in MAVLink
}

var fieldCache sync.Map // reflect.Type → []field

// fieldsOf returns the MAVLink field names of a message struct type.
func fieldsOf(typ reflect.Type) []field {
	if f, ok := fieldCache.Load(typ); ok {
		return f.([]field)
	}
	fields := make([]field, typ.NumField())
	for i := range typ.NumField() {
		sf := typ.Field(i)
		name := sf.Tag.Get("mavname")
		if name == "" {
			name = goToDef(sf.Name, strings.ToLower)
		}
		bits := 0
		if w := sf.Tag.Get("mavenum"); w != "" {
			bits, _ = strconv.Atoi(strings.TrimLeft(w, "uint")) // "uint8" → 8
		}
		fields[i] = field{index: i, name: name, bits: bits}
	}
	fieldCache.Store(typ, fields)
	return fields
}

var messageTypes = sync.OnceValue(func() map[uint32]reflect.Type {
	types := map[uint32]reflect.Type{}
	for _, m := range ardupilotmega.Dialect.Messages {
		types[m.GetID()] = reflect.TypeOf(m).Elem()
	}
	return types
})

// marshalPayload encodes a message's fields as a JSON object. Values are kept
// in their own Go types, so enums are written as numbers (not gomavlib's text
// names) and float32 values without float64 rounding noise.
func marshalPayload(m message.Message) (json.RawMessage, error) {
	v := reflect.ValueOf(m).Elem()
	out := make(map[string]any, v.NumField())
	for _, f := range fieldsOf(v.Type()) {
		out[f.name] = plainValue(v.Field(f.index))
	}
	return json.Marshal(out)
}

func plainValue(v reflect.Value) any {
	switch v.Kind() {
	case reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return v.Uint()
	case reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int()
	case reflect.Float32, reflect.Float64:
		f := v.Float()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil
		}
		if v.Kind() == reflect.Float32 {
			return float32(f)
		}
		return f
	case reflect.String:
		return v.String()
	case reflect.Array:
		a := make([]any, v.Len())
		for i := range a {
			a[i] = plainValue(v.Index(i))
		}
		return a
	}
	panic(fmt.Sprintf("schema: unsupported field kind %v", v.Kind()))
}

// setValue sets v from a decoded JSON value. bits, if not 0, is the field's
// width on the wire, which is narrower than v for enums.
func setValue(v reflect.Value, raw any, bits int) error {
	switch v.Kind() {
	case reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Float32, reflect.Float64:
		if bits == 0 {
			bits = v.Type().Bits()
		}
	}
	switch v.Kind() {
	case reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := number(raw)
		if err != nil {
			return err
		}
		u, err := strconv.ParseUint(n.String(), 10, bits)
		if err != nil {
			return err
		}
		v.SetUint(u)
	case reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := number(raw)
		if err != nil {
			return err
		}
		i, err := strconv.ParseInt(n.String(), 10, bits)
		if err != nil {
			return err
		}
		v.SetInt(i)
	case reflect.Float32, reflect.Float64:
		if raw == nil {
			v.SetFloat(math.NaN())
			return nil
		}
		n, err := number(raw)
		if err != nil {
			return err
		}
		f, err := strconv.ParseFloat(n.String(), bits)
		if err != nil {
			return err
		}
		v.SetFloat(f)
	case reflect.String:
		s, ok := raw.(string)
		if !ok {
			return fmt.Errorf("want a string, got %T", raw)
		}
		v.SetString(s)
	case reflect.Array:
		a, ok := raw.([]any)
		if !ok || len(a) != v.Len() {
			return fmt.Errorf("want an array of %d, got %v", v.Len(), raw)
		}
		for i := range a {
			if err := setValue(v.Index(i), a[i], 0); err != nil {
				return fmt.Errorf("[%d]: %w", i, err)
			}
		}
	default:
		return fmt.Errorf("unsupported field kind %v", v.Kind())
	}
	return nil
}

func number(raw any) (json.Number, error) {
	n, ok := raw.(json.Number)
	if !ok {
		return "", fmt.Errorf("want a number, got %T", raw)
	}
	return n, nil
}
