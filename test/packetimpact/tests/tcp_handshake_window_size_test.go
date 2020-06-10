// Copyright 2020 The gVisor Authors.
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

package tcp_handshake_window_size_test

import (
	"flag"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/test/packetimpact/testbench"
)

func init() {
	testbench.RegisterFlags(flag.CommandLine)
}

// TestTCPHandshakeWindowSize tests if the stack is honoring the window size
// communicated during handshake.
func TestTCPHandshakeWindowSize(t *testing.T) {
	dut := testbench.NewDUT(t)
	defer dut.TearDown()
	listenFD, remotePort := dut.CreateListener(unix.SOCK_STREAM, unix.IPPROTO_TCP, 1)
	defer dut.Close(listenFD)
	conn := testbench.NewTCPIPv4(t, testbench.TCP{DstPort: &remotePort}, testbench.TCP{SrcPort: &remotePort})
	defer conn.Close()

	// Start handshake with zero window size.
	conn.Send(testbench.TCP{Flags: testbench.Uint8(header.TCPFlagSyn), WindowSize: testbench.Uint16(uint16(0))})
	if _, err := conn.ExpectData(&testbench.TCP{Flags: testbench.Uint8(header.TCPFlagSyn | header.TCPFlagAck)}, nil, time.Second); err != nil {
		t.Fatalf("expected SYN-ACK %s", err)
	}
	// Update the advertized window size with the ACK, completing the handshake.
	// Ensure the window size has MSB set and is treated as unsigned value.
	conn.Send(testbench.TCP{Flags: testbench.Uint8(header.TCPFlagAck), WindowSize: testbench.Uint16(uint16(1 << 15))})

	acceptFd, _ := dut.Accept(listenFD)
	defer dut.Close(acceptFd)

	sampleData := []byte("Sample Data")
	samplePayload := &testbench.Payload{Bytes: sampleData}

	// Expect the dut to honor the recently advertised window-size and send
	// the expected payload.
	dut.Send(acceptFd, sampleData, 0)
	if _, err := conn.ExpectData(&testbench.TCP{}, samplePayload, time.Second); err != nil {
		t.Fatalf("expected a packet with payload %v: %s", samplePayload, err)
	}
}
