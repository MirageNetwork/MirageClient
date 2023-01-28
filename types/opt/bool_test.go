// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package opt

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestBool(t *testing.T) {
	tests := []struct {
		name     string
		in       any
		want     string // JSON
		wantBack any
	}{
		{
			name: "null_for_unset",
			in: struct {
				True  Bool
				False Bool
				Unset Bool
			}{
				True:  "true",
				False: "false",
			},
			want: `{"True":true,"False":false,"Unset":null}`,
			wantBack: struct {
				True  Bool
				False Bool
				Unset Bool
			}{
				True:  "true",
				False: "false",
				Unset: "unset",
			},
		},
		{
			name: "omitempty_unset",
			in: struct {
				True  Bool
				False Bool
				Unset Bool `json:",omitempty"`
			}{
				True:  "true",
				False: "false",
			},
			want: `{"True":true,"False":false}`,
		},
		{
			name: "unset_marshals_as_null",
			in: struct {
				True  Bool
				False Bool
				Foo   Bool
			}{
				True:  "true",
				False: "false",
				Foo:   "unset",
			},
			want: `{"True":true,"False":false,"Foo":null}`,
			wantBack: struct {
				True  Bool
				False Bool
				Foo   Bool
			}{"true", "false", "unset"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			j, err := json.Marshal(tt.in)
			if err != nil {
				t.Fatal(err)
			}
			if string(j) != tt.want {
				t.Errorf("wrong JSON:\n got: %s\nwant: %s\n", j, tt.want)
			}

			wantBack := tt.in
			if tt.wantBack != nil {
				wantBack = tt.wantBack
			}
			// And back again:
			newVal := reflect.New(reflect.TypeOf(tt.in))
			out := newVal.Interface()
			if err := json.Unmarshal(j, out); err != nil {
				t.Fatalf("Unmarshal %#q: %v", j, err)
			}
			got := newVal.Elem().Interface()
			if !reflect.DeepEqual(got, wantBack) {
				t.Errorf("value mismatch\n got: %+v\nwant: %+v\n", got, wantBack)
			}
		})
	}
}

func TestBoolEqualBool(t *testing.T) {
	tests := []struct {
		b    Bool
		v    bool
		want bool
	}{
		{"", true, false},
		{"", false, false},
		{"sdflk;", true, false},
		{"sldkf;", false, false},
		{"true", true, true},
		{"true", false, false},
		{"false", true, false},
		{"false", false, true},
		{"1", true, false},    // "1" is not true; only "true" is
		{"True", true, false}, // "True" is not true; only "true" is
	}
	for _, tt := range tests {
		if got := tt.b.EqualBool(tt.v); got != tt.want {
			t.Errorf("(%q).EqualBool(%v) = %v; want %v", string(tt.b), tt.v, got, tt.want)
		}
	}
}
