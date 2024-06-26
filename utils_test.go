// Copyright 2023-2024 antlabs. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package greatws

import (
	"reflect"
	"testing"
)

func TestStringToBytes(t *testing.T) {
	type args struct {
		s string
	}
	tests := []struct {
		name  string
		args  args
		wantB []byte
	}{
		{
			name:  "test1",
			args:  args{s: "test1"},
			wantB: []byte("test1"),
		},
		{
			name:  "test2",
			args:  args{s: "test2"},
			wantB: []byte("test2"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if gotB := StringToBytes(tt.args.s); !reflect.DeepEqual(gotB, tt.wantB) {
				t.Errorf("StringToBytes() = %v, want %v", gotB, tt.wantB)
			}
		})
	}
}

func Test_secWebSocketAccept(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{name: ">0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := secWebSocketAccept(); len(got) == 0 {
				t.Errorf("secWebSocketAccept() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_secWebSocketAcceptVal(t *testing.T) {
	type args struct {
		val string
	}
	tests := []struct {
		name string
		args args
		want string
	}{
		{name: "test1", args: args{val: "test1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := secWebSocketAcceptVal(tt.args.val); len(got) == 0 {
				t.Errorf("secWebSocketAcceptVal() = %v, want %v", got, tt.want)
			}
		})
	}
}
