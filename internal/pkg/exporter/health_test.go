/**
# Copyright (c) Advanced Micro Devices, Inc. All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the \"License\");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an \"AS IS\" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
**/

package exporter

import "testing"

func TestNormalizeDeviceID(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want string
	}{
		{
			name: "physical PCI device",
			id:   "0000:07:00.0",
			want: "0000:07:00.0",
		},
		{
			name: "replicated PCI device",
			id:   "0000:07:00.0-3",
			want: "0000:07:00.0",
		},
		{
			name: "physical partition device",
			id:   "amdgpu_xcp_30",
			want: "amdgpu_xcp_30",
		},
		{
			name: "replicated partition device",
			id:   "amdgpu_xcp_30-3",
			want: "amdgpu_xcp_30",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeDeviceID(tt.id); got != tt.want {
				t.Fatalf("normalizeDeviceID(%q) = %q, want %q", tt.id, got, tt.want)
			}
		})
	}
}
