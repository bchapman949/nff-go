// Copyright 2017 Intel Corporation.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/intel-go/nff-go/flow"
	"github.com/intel-go/nff-go/packet"
	"github.com/intel-go/nff-go/test/stability/stabilityCommon"
)

// testScenario=1:
// Firstly packets are generated with different UDP destination addresses,
// filled with IPv4 and UDP checksums and sent to 0 port.
// Secondly packets are received on 0 and 1 ports, verified and calculated.
// Expects to get exact proportion of input packets.
//
// testScenario=2:
// Packets are received on 0 port (from first part), divided according to testType
// and sent back for checking.
//
// testScenario=0:
// Combination of above parts at one machine. Is used by go test.
// Packets are generated, divided and calculated in the same pipeline.

const (
	gotest uint = iota
	generatePart
	receivePart
)

const (
	separate uint = iota
	split
	partition
)

var (
	totalPackets uint64 = 10000000
	// Payload is 16 byte md5 hash sum of headers
	payloadSize uint   = 16
	speed       uint64 = 1000000
	passedLimit uint64 = 85

	recvPacketsGroup1 uint64
	recvPacketsGroup2 uint64

	count         uint64
	countRes      uint64
	recvPackets   uint64
	brokenPackets uint64

	dstPort1 uint16 = 111
	dstPort2 uint16 = 222
	dstPort3 uint16 = 333

	testDoneEvent *sync.Cond
	progStart     time.Time

	// T second timeout is used to let generator reach required speed
	// During timeout packets are skipped and not counted
	T = 10 * time.Second

	outport1     uint
	outport2     uint
	inport1      uint
	inport2      uint
	dpdkLogLevel = "--log-level=0"

	fixMACAddrs  func(*packet.Packet, flow.UserContext)
	fixMACAddrs1 func(*packet.Packet, flow.UserContext)
	fixMACAddrs2 func(*packet.Packet, flow.UserContext)

	rulesString       = ""
	filename          = &rulesString
	passed      int32 = 1
	l3Rules     *packet.L3Rules

	gTestType uint
	lessPercent uint
	eps uint
)

func main() {
	var testScenario uint
	var testType uint
	flag.UintVar(&testScenario, "testScenario", 0, "1 to use 1 as GENERATE part, 2 to use as RECEIVE part, 0 to use as ONE-MACHINE variant")
	flag.UintVar(&testType, "testType", 0, "0 to use Separate test, 1 to use Split test, 2 to use Partition test")
	flag.Uint64Var(&passedLimit, "passedLimit", passedLimit, "received/sent minimum ratio to pass test")
	flag.Uint64Var(&speed, "speed", speed, "speed of generator, Pkts/s")
	flag.UintVar(&outport1, "outport1", 0, "port for 1st sender")
	flag.UintVar(&outport2, "outport2", 1, "port for 2nd sender")
	flag.UintVar(&inport1, "inport1", 0, "port for 1st receiver")
	flag.UintVar(&inport2, "inport2", 1, "port for 2nd receiver")
	flag.Uint64Var(&totalPackets, "number", totalPackets, "total number of packets to receive by test")
	flag.DurationVar(&T, "timeout", T, "test start delay, needed to stabilize speed. Packets sent during timeout do not affect test result")
	configFile := flag.String("config", "", "Specify json config file name (mandatory for VM)")
	target := flag.String("target", "", "Target host name from config file (mandatory for VM)")
	filename = flag.String("FILE", rulesString, "file with split rules in .conf format. If you change default port numbers, please, provide modified rules file too")
	dpdkLogLevel = *(flag.String("dpdk", "--log-level=0", "Passes an arbitrary argument to dpdk EAL"))
	flag.Parse()
	if err := executeTest(*configFile, *target, testScenario, testType); err != nil {
		fmt.Printf("fail: %+v\n", err)
	}
}

func executeTest(configFile, target string, testScenario uint, testType uint) error {
	if testScenario > 3 || testScenario < 0 {
		return errors.New("testScenario should be in interval [0, 3]")
	}
	gTestType = testType
	if testType == separate {
		rulesString = "test-separate-l3rules.conf"
		// Test expects to receive 33% of packets on 0 port and 66% on 1 port
		lessPercent = 33
		eps = 2
	} else if testType == split {
		rulesString = "test-split.conf"
                // Test expects to receive 20% of packets on 0 port and 80% on 1 port
                lessPercent = 20
                eps = 4
	} else if testType == partition {
                // Test expects to receive 10% of packets on 0 port and 90% on 1 port
                lessPercent = 10
                eps = 3
	}
	// Init NFF-GO system
	config := flow.Config{
		DPDKArgs: []string{dpdkLogLevel},
	}
	if err := flow.SystemInit(&config); err != nil {
		return err
	}
	stabilityCommon.InitCommonState(configFile, target)
	fixMACAddrs = stabilityCommon.ModifyPacket[outport1].(func(*packet.Packet, flow.UserContext))
	fixMACAddrs1 = stabilityCommon.ModifyPacket[outport1].(func(*packet.Packet, flow.UserContext))
	fixMACAddrs2 = stabilityCommon.ModifyPacket[outport2].(func(*packet.Packet, flow.UserContext))

	var flow1 *flow.Flow
	var flow2 *flow.Flow
	if testType != partition && testScenario != generatePart {
		var err error
		// Get splitting rules from access control file.
		l3Rules, err = packet.GetL3ACLFromORIG(*filename)
		if err != nil {
			return err
		}
	}

	if testScenario == receivePart {
		inputFlow, err := flow.SetReceiver(uint8(inport1))
		if err != nil {
			return err
		}

		if testType == separate {
			// Separate packet flow based on ACL.
			flow2, err = flow.SetSeparator(inputFlow, l3Separator, nil)
			flow1 = inputFlow
		} else if testType == split {
			// Split packet flow based on ACL.
			var splittedFlows []*flow.Flow
			splittedFlows, err = flow.SetSplitter(inputFlow, l3Splitter, 3, nil)
			// "0" flow is used for dropping packets without sending them.
			if err1 := flow.SetStopper(splittedFlows[0]); err1 != nil {
				return err1
			}
			flow1 = splittedFlows[1]
			flow2 = splittedFlows[2]
		} else if testType == partition {
			// Parition flow
			flow2, err = flow.SetPartitioner(inputFlow, 100, 1000)
			flow1 = inputFlow
		}
		if err != nil {
			return err
		}

		if err := flow.SetHandler(flow1, fixPackets1, nil); err != nil {
			return err
		}
		if err := flow.SetHandler(flow2, fixPackets2, nil); err != nil {
			return err
		}

		// Send each flow to corresponding port. Send queues will be added automatically.
		if err := flow.SetSender(flow1, uint8(outport1)); err != nil {
			return err
		}
		if err := flow.SetSender(flow2, uint8(outport2)); err != nil {
			return err
		}

		// Begin to process packets.
		if err := flow.SystemStart(); err != nil {
			return err
		}
	} else {
		var m sync.Mutex
		testDoneEvent = sync.NewCond(&m)

		// Create packet flow
		generatedFlow, err := flow.SetFastGenerator(generatePacket, speed, nil)
		if err != nil {
			return err
		}
		var flow1, flow2 *flow.Flow
		if testScenario == generatePart {
			if err := flow.SetSender(generatedFlow, uint8(outport1)); err != nil {
				return err
			}

			// Create receiving flows and set a checking function for it
			flow1, err = flow.SetReceiver(uint8(inport1))
			if err != nil {
				return err
			}
			flow2, err = flow.SetReceiver(uint8(inport2))
			if err != nil {
				return err
			}
		} else {
			if testType == separate {
				// Separate packet flow based on ACL.
				flow2, err = flow.SetSeparator(generatedFlow, l3Separator, nil)
				flow1 = generatedFlow
			} else if testType == split {
				// Split packet flow based on ACL.
				var splittedFlows []*flow.Flow
				splittedFlows, err = flow.SetSplitter(generatedFlow, l3Splitter, 3, nil)
				// "0" flow is used for dropping packets without sending them.
				if err1 := flow.SetStopper(splittedFlows[0]); err1 != nil {
					return err1
				}
				flow1 = splittedFlows[1]
				flow2 = splittedFlows[2]
			} else if testType == partition {
				// Partition flow
				flow2, err = flow.SetPartitioner(generatedFlow, 100, 1000)
				flow1 = generatedFlow
			}
			if err != nil {
				return err
			}
			if err := flow.SetHandler(flow1, fixPackets1, nil); err != nil {
				return err
			}
			if err := flow.SetHandler(flow2, fixPackets2, nil); err != nil {
				return err
			}
		}
		if err := flow.SetHandler(flow1, checkInputFlow1, nil); err != nil {
			return err
		}
		if err := flow.SetHandler(flow2, checkInputFlow2, nil); err != nil {
			return err
		}

		if err := flow.SetStopper(flow1); err != nil {
			return err
		}
		if err := flow.SetStopper(flow2); err != nil {
			return err
		}

		// Start pipeline
		go func() {
			err = flow.SystemStart()
		}()
		if err != nil {
			return err
		}
		progStart = time.Now()

		// Wait for enough packets to arrive
		testDoneEvent.L.Lock()
		testDoneEvent.Wait()
		testDoneEvent.L.Unlock()
		return composeStatistics()
	}
	return nil
}

func composeStatistics() error {
	// Compose statistics
	sent := atomic.LoadUint64(&countRes)

	recv1 := atomic.LoadUint64(&recvPacketsGroup1)
	recv2 := atomic.LoadUint64(&recvPacketsGroup2)
	received := recv1 + recv2

	var p1 uint
	var p2 uint
	if received != 0 {
		p1 = uint(recv1 * 100 / received)
		p2 = uint(recv2 * 100 / received)
	}
	broken := atomic.LoadUint64(&brokenPackets)

	// Print report
	println("Sent", sent, "packets")
	println("Received", received, "packets")
	println("Ratio =", received*100/sent, "%")
	println("On port", inport1, "received=", recv1, "packets")
	println("On port", inport2, "received=", recv2, "packets")
	println("Proportion of packets received on", inport1, "port =", p1, "%")
	println("Proportion of packets received on", inport2, "port =", p2, "%")

	println("Broken = ", broken, "packets")

	// Test is passed, if p1 is ~lessPercent% and p2 is ~100 - lessPercent%
	// and if total receive/send rate is high
	if atomic.LoadInt32(&passed) != 0 &&
		p1 <= lessPercent + eps && p2 <= 100 - lessPercent + eps &&
		p1 >= lessPercent - eps && p2 >= 100 - lessPercent - eps && received*100/sent > passedLimit {
		println("TEST PASSED")
		return nil
	}
	println("TEST FAILED")
	return errors.New("final statistics check failed")
}

// Function to use in generator
func generatePacket(pkt *packet.Packet, context flow.UserContext) {
	if pkt == nil {
		log.Fatal("Failed to create new packet")
	}
	if packet.InitEmptyIPv4UDPPacket(pkt, payloadSize) == false {
		log.Fatal("Failed to init empty packet")
	}
	ipv4 := pkt.GetIPv4()
	udp := pkt.GetUDPForIPv4()

	if gTestType == separate {
		// Generate packets of 3 groups
		if count%3 == 0 {
			udp.DstPort = packet.SwapBytesUint16(dstPort1)
		} else if count%3 == 1 {
			udp.DstPort = packet.SwapBytesUint16(dstPort2)
		} else {
			udp.DstPort = packet.SwapBytesUint16(dstPort3)
		}
	} else if gTestType == split {
		// Generate packets of 2 groups
		if count%5 == 0 {
			udp.DstPort = packet.SwapBytesUint16(dstPort1)
		} else {
			udp.DstPort = packet.SwapBytesUint16(dstPort2)
		}
	} else if gTestType == partition {
		// Generate identical packets
		udp.DstPort = packet.SwapBytesUint16(dstPort1)
	}
	ipv4.HdrChecksum = packet.SwapBytesUint16(packet.CalculateIPv4Checksum(ipv4))
	udp.DgramCksum = packet.SwapBytesUint16(packet.CalculateIPv4UDPChecksum(ipv4, udp, pkt.Data))
	fixMACAddrs(pkt, context)
	atomic.AddUint64(&count, 1)

	// We do not consider the start time of the system in this test
	if time.Since(progStart) >= T && atomic.LoadUint64(&recvPackets) < totalPackets {
		atomic.AddUint64(&countRes, 1)
	}
}

func checkInputFlow1(pkt *packet.Packet, context flow.UserContext) {
	if time.Since(progStart) < T || stabilityCommon.ShouldBeSkipped(pkt) {
		return
	}
	if atomic.AddUint64(&recvPackets, 1) > totalPackets {
		testDoneEvent.Signal()
		return
	}

	pkt.ParseData()
	ipv4 := pkt.GetIPv4()
	udp := pkt.GetUDPForIPv4()
	recvIPv4Cksum := packet.SwapBytesUint16(packet.CalculateIPv4Checksum(ipv4))
	recvUDPCksum := packet.SwapBytesUint16(packet.CalculateIPv4UDPChecksum(ipv4, udp, pkt.Data))
	if recvIPv4Cksum != ipv4.HdrChecksum || recvUDPCksum != udp.DgramCksum {
		// Packet is broken
		atomic.AddUint64(&brokenPackets, 1)
		return
	}
	if udp.DstPort != packet.SwapBytesUint16(dstPort1) {
		println("Unexpected packet in inputFlow1")
		println("TEST FAILED")
		atomic.StoreInt32(&passed, 0)
		return
	}
	atomic.AddUint64(&recvPacketsGroup1, 1)
}

func checkInputFlow2(pkt *packet.Packet, context flow.UserContext) {
	if time.Since(progStart) < T || stabilityCommon.ShouldBeSkipped(pkt) {
		return
	}
	if atomic.AddUint64(&recvPackets, 1) > totalPackets {
		testDoneEvent.Signal()
		return
	}

	pkt.ParseData()
	ipv4 := pkt.GetIPv4()
	udp := pkt.GetUDPForIPv4()
	recvIPv4Cksum := packet.SwapBytesUint16(packet.CalculateIPv4Checksum(ipv4))
	recvUDPCksum := packet.SwapBytesUint16(packet.CalculateIPv4UDPChecksum(ipv4, udp, pkt.Data))
	if recvIPv4Cksum != ipv4.HdrChecksum || recvUDPCksum != udp.DgramCksum {
		// Packet is broken
		atomic.AddUint64(&brokenPackets, 1)
		return
	}
	if gTestType == separate &&
		udp.DstPort != packet.SwapBytesUint16(dstPort2) &&
		udp.DstPort != packet.SwapBytesUint16(dstPort3) ||
		gTestType == split &&
			udp.DstPort != packet.SwapBytesUint16(dstPort2) ||
		gTestType == partition &&
			udp.DstPort != packet.SwapBytesUint16(dstPort1) {
		println("Unexpected packet in inputFlow2")
		println("TEST FAILED")
		atomic.StoreInt32(&passed, 0)
		return
	}
	atomic.AddUint64(&recvPacketsGroup2, 1)
}

func l3Separator(pkt *packet.Packet, context flow.UserContext) bool {
	// Return whether packet is accepted
	return pkt.L3ACLPermit(l3Rules)
}

func l3Splitter(currentPacket *packet.Packet, context flow.UserContext) uint {
	// Return number of flow to which put this packet. Based on ACL rules.
	return currentPacket.L3ACLPort(l3Rules)
}

func fixPackets1(pkt *packet.Packet, ctx flow.UserContext) {
	if stabilityCommon.ShouldBeSkipped(pkt) {
		return
	}
	fixMACAddrs1(pkt, ctx)
}

func fixPackets2(pkt *packet.Packet, ctx flow.UserContext) {
	if stabilityCommon.ShouldBeSkipped(pkt) {
		return
	}
	fixMACAddrs2(pkt, ctx)
}
