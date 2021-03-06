// this code is largely based on Michael Brown's excellent Python API
// https://github.com/mjbrown/bgapi

package bgapi

// TODO take care of some initialization

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/tarm/serial"
)

const (
	defaultTimeoutMs = 1000
)

// Mac represents an IEEE MAC address
type Mac [6]byte

// QualifiedMac represents an IEEE MAC address qualified by BLE MAC Type idenfier
type QualifiedMac struct {
	Address  Mac
	AddrType byte
}

// Hashable return a hashable representation of the address
func (qm *QualifiedMac) Hashable() string {
	return string(append(qm.Address[:], qm.AddrType))
}

// ConnectionParameters connection parameters
type ConnectionParameters struct {
	IntervalMin uint16
	intervalMax uint16
	Timeout     uint16
	Latency     uint16
}

// SystemCounters result of query for system diagnostic counters
type SystemCounters struct {
	Txok, Txretry, Rxok, Rxfail, Mbuf byte
}

// SystemInfo result of system information query
type SystemInfo struct {
	Major, Minor, Patch, Build, LLVersion uint16
	ProtocolVersion, HW                   byte
}

// ConnectionStatus BLE connection status
type ConnectionStatus struct {
	Connection, Flags              byte
	Address                        QualifiedMac
	ConnInterval, Timeout, Latency uint16
	Bonding                        byte
}

// ConnectionVersionIndication indicates version data
type ConnectionVersionIndication struct {
	Connection, Version byte
	CompID, SubVersion  uint16
}

// SmBondStatus security manager bonding status
type SmBondStatus struct {
	Bond, KeySize, MITM, Keys byte
}

// GapScanRespone GAP scan response indication
type GapScanRespone struct {
	RSSI       int8
	PacketType byte
	Address    QualifiedMac
	Bond       byte
	Data       []byte
}

// SpiConfig SPI configuration parameters
type SpiConfig struct {
	Polarity byte
	Phase    byte
	BitOrder byte
	BaudE    byte
	BaudM    byte
}

// IoPortStatus IO Port Status info
type IoPortStatus struct {
	Timestamp        uint32
	Port, Irq, State byte
}

// Delegate an API Delegate to be implemented by clients of this module
type Delegate interface {
	// OnSystemBoot invoked when the BLED112 boots
	OnSystemBoot(info *SystemInfo)
	// OnSystemDebug invoked when BLED112 generates debug reply
	OnSystemDebug(data []byte)
	// OnSystemEndpointWatermarkRx inovked when receiveing Endpoint Watermark
	OnSystemEndpointWatermarkRx(endpoint byte, data byte)
	// OnSystemEndpointWatermarkTx inovked when transmitting Endpoint Watermark
	OnSystemEndpointWatermarkTx(endpoint byte, data byte)
	// OnSystemScriptFailure invoked on script failure
	OnSystemScriptFailure(addr uint16, reason uint16)
	// OnSystemNoLicenseKey invoked when no license key is found
	OnSystemNoLicenseKey()
	// OnFlashPsKey invoked when flash PS Key is updated
	OnFlashPsKey(key uint16, value []byte)
	// OnAttributeValue invoked when attribute value changes
	OnAttributeValue(connection byte, reason byte, handle uint16, offset uint16, value []byte)
	// OnAttributeUserReadRequest inovked by user read request
	OnAttributeUserReadRequest(connection byte, handle uint16, offset uint16, maxSize byte)
	// OnAttributeStatus invoked when status changes
	OnAttributeStatus(handle uint16, flags byte)
	// OnConnectionStatus invoked when the connection status changes
	OnConnectionStatus(status *ConnectionStatus)
	// OnConnectionVersionIndication invoked when version indication is updated
	OnConnectionVersionIndication(ind *ConnectionVersionIndication)
	// OnConnectionFeatureIndication invoked when feature indication is updated
	OnConnectionFeatureIndication(connection byte, features []byte)
	// OnConnectionRawRx invoked when raw data is received
	OnConnectionRawRx(connection byte, data []byte)
	// OnConnectionDisconnected invoked when the connection is lost
	OnConnectionDisconnected(connection byte, reason uint16)
	// OnAttrclientIndicated inovked when an attribute is indicated
	OnAttrclientIndicated(connection byte, attrHandle uint16)
	// OnAttrclientProcedureCompleted invoked upon procedure completion
	OnAttrclientProcedureCompleted(connection byte, result uint16, chrHandle uint16)
	// OnAttrclientGroupFound invoked when the group is found
	OnAttrclientGroupFound(connection byte, start uint16, end uint16, uuid []byte)
	// OnAttrclientAttributeFound invoked when the attribute is found
	OnAttrclientAttributeFound(connection byte, chrdecl uint16, value uint16, properties byte, uuid []byte)
	// OnAttrclientFindInformationFound invoked when information is available
	OnAttrclientFindInformationFound(connection byte, chrHandle uint16, uuid []byte)
	// OnAttrclientAttributeValue invoked when value changes
	OnAttrclientAttributeValue(connection byte, attHandle uint16, valueType byte, value []byte)
	// OnAttrclientReadMultipleResponse invoked when the client responds
	OnAttrclientReadMultipleResponse(connection byte, handles []byte)
	// OnGapScanResponse invoked when GAP Scan Response is available
	OnGapScanResponse(resp *GapScanRespone)
	// OnGapModeChanged invoked when the GAP mode changes
	OnGapModeChanged(discover byte, connect byte)
	// OnSmSmpData invoked when security manager data is posted
	OnSmSmpData(handle byte, packet byte, data []byte)
	// OnSmBondingFail invoked when the bonding fails
	OnSmBondingFail(handle byte, result uint16)
	// OnSmPasskeyDisplay inovked when the paskey is displayed
	OnSmPasskeyDisplay(handle byte, passkey uint32)
	// OnSmPasskeyRequest invoked when the paskey is requested
	OnSmPasskeyRequest(handle byte)
	// OnSmBondStatus invoked when the bond status is updated
	OnSmBondStatus(status *SmBondStatus)
	// OnHardwareIoPortStatus invoked when the IO port status is changed
	OnHardwareIoPortStatus(status *IoPortStatus)
	// OnHardwareSoftTimer invoked upon soft timer expiry
	OnHardwareSoftTimer(handle byte)
	// OnHardwareAdcResult invoked when ADC result is generated
	OnHardwareAdcResult(input byte, value int16)
}

// LoggingDelegate a delegate that implements a simple logger
type LoggingDelegate struct {
}

//
// frame header
//

type bgFrameHeader struct {
	length        uint16
	packetClass   uint8
	packetCommand uint8
}

func (hdr *bgFrameHeader) frameLengthGet() int {
	return int(hdr.length & 0x7fff)
}

func (hdr *bgFrameHeader) messageTypeGet() int {
	return int(hdr.length >> 15)
}

func (hdr *bgFrameHeader) technologyTypeGet() int {
	return int((hdr.length >> 11) & 0xf)
}

type bgFrameReader struct {
	buf     *bytes.Buffer
	header  bgFrameHeader
	inFrame bool
}

// append raw data
func (fr *bgFrameReader) append(data []byte) {
	fr.buf.Write(data)
}

// HasFrame true if at least one frame is ready to be extracted
func (fr *bgFrameReader) hasFrame() bool {
	if !fr.inFrame && (fr.buf.Len() >= 4) {
		// extract the header
		binary.Read(fr.buf, binary.LittleEndian, &fr.header)
		fr.inFrame = true
	}

	return fr.inFrame && (fr.buf.Len() >= fr.header.frameLengthGet())
}

// Next read the next pending frame
func (fr *bgFrameReader) next() ([]byte, *bgFrameHeader) {
	if !fr.inFrame {
		return nil, nil
	}
	fr.inFrame = false

	return fr.buf.Next(fr.header.frameLengthGet()), &fr.header
}

type operation struct {
	class      byte
	cmd        byte
	completion func(*bytes.Buffer, error)
	txData     []byte
	timeout    time.Duration
}

// API for low-level BLED112 access
type API struct {
	ser       *serial.Port
	txC       chan *operation
	rxReplyC  chan error
	pendingOp *operation
	delegate  Delegate
	framer    bgFrameReader
}

func boolCast(boolean bool) byte {
	if boolean {
		return 1
	}

	return 0
}

// NewAPI returns a new API structure
func NewAPI(delegate Delegate) *API {
	var api = API{delegate: delegate}
	return &api
}

// OpenBLED112 open the conneciton to the BLED112
func (api *API) OpenBLED112(port string) {
	cfg := serial.Config{Name: port, Baud: 115200}
	if ser, err := serial.OpenPort(&cfg); err == nil {
		api.ser = ser

		// handle receiving data
		go func() {
			var data = make([]byte, 128)
			for true {
				if n, err := api.ser.Read(data); err == nil {
					api.onSerialPortData(data[:n])
				}
			}
		}()

		go func() {
			for true {
				op := <-api.txC
				// FIXME need to handle errors
				api.ser.Write(op.txData)
				api.ser.Flush()

				select {
				case _ = <-api.rxReplyC:
					// reply received, continue
				case <-time.After(op.timeout * time.Millisecond):
					op.completion(nil, errors.New("operation timed-out"))
				}
			}
		}()
	}
}

func (api *API) sendWithTimeout(class byte, cmd byte, data []byte, timeoutMs time.Duration, completion func(*bytes.Buffer)) error {
	// encode the command

	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, class)
	binary.Write(buf, binary.LittleEndian, cmd)
	binary.Write(buf, binary.LittleEndian, data)

	var ret error
	api.txC <- &operation{class: class, cmd: cmd, txData: data, timeout: timeoutMs,
		completion: func(buf *bytes.Buffer, err error) {
			if err == nil {
				completion(buf)
			}
			ret = err
		},
	}

	return ret
}

func (api *API) send(class byte, cmd byte, data []byte, completion func(*bytes.Buffer)) error {
	return api.sendWithTimeout(class, cmd, data, defaultTimeoutMs, completion)
}

// handle receiveing data from the serial port
func (api *API) onSerialPortData(data []byte) {
	api.framer.append(data)
	for api.framer.hasFrame() {
		frame, hdr := api.framer.next()
		buf := bytes.NewBuffer(frame)
		switch hdr.messageTypeGet() {
		case 0:
			if api.pendingOp != nil {
				var err error
				if (api.pendingOp.class != hdr.packetClass) || (api.pendingOp.cmd != hdr.packetCommand) {
					err = errors.New("received incorrect response type")
				}
				api.pendingOp.completion(buf, err)
				api.rxReplyC <- nil
			} else {
				fmt.Println("FIXME received bad header!")
			}
		case 1:
			api.parseEvent(hdr, buf)
		}
	}
}

// SystemReset perform module reset
func (api *API) SystemReset(bootInDfu bool, completion func()) error {
	data := []byte{boolCast(bootInDfu)}
	return api.send(0, 0, data, func(*bytes.Buffer) {
		completion()
	})
}

// SystemHello say hello
func (api *API) SystemHello(completion func()) error {
	return api.send(0, 1, []byte{}, func(*bytes.Buffer) {
		completion()
	})
}

// SystemAddressGet get the address
func (api *API) SystemAddressGet(completion func(Mac)) error {
	return api.send(0, 2, []byte{}, func(buf *bytes.Buffer) {
		var mac Mac
		binary.Read(buf, binary.LittleEndian, mac)
		completion(mac)
	})
}

// SystemRegWrite write device register
func (api *API) SystemRegWrite(addr uint16, value uint8, completion func(uint16)) error {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, addr)
	binary.Write(buf, binary.LittleEndian, value)
	return api.send(0, 3, buf.Bytes(), func(buf *bytes.Buffer) {
		var value uint16
		binary.Read(buf, binary.LittleEndian, &value)
		completion(value)
	})
}

// SystemRegRead read device register
func (api *API) SystemRegRead(addr uint16, completion func(uint16, uint8)) error {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, addr)
	return api.send(0, 4, []byte{}, func(buf *bytes.Buffer) {
		var rxAddr uint16
		var value uint8
		binary.Read(buf, binary.LittleEndian, &rxAddr)
		binary.Read(buf, binary.LittleEndian, &value)
		completion(rxAddr, value)
	})
}

// SystemCountersGet get the counters
func (api *API) SystemCountersGet(completion func(*SystemCounters)) error {
	return api.send(0, 5, []byte{}, func(buf *bytes.Buffer) {
		var counters = SystemCounters{}
		binary.Read(buf, binary.LittleEndian, &counters)
		completion(&counters)
	})
}

// SystemConnectionsGet get the connections
func (api *API) SystemConnectionsGet(completion func(uint8)) error {
	return api.send(0, 6, []byte{}, func(buf *bytes.Buffer) {
		var maxConn uint8
		binary.Read(buf, binary.LittleEndian, &maxConn)
		completion(maxConn)
	})
}

// SystemMemoryRead read memory
func (api *API) SystemMemoryRead(addr uint16, length uint8, completion func(uint32, []byte)) error {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, addr)
	binary.Write(buf, binary.LittleEndian, length)
	return api.send(0, 7, buf.Bytes(), func(buf *bytes.Buffer) {
		var rxAddr uint32
		var dataLen uint8
		binary.Read(buf, binary.LittleEndian, &rxAddr)
		binary.Read(buf, binary.LittleEndian, &dataLen)
		completion(rxAddr, buf.Bytes())
	})
}

// SystemInfoGet get system informaiton
func (api *API) SystemInfoGet(completion func(*SystemInfo)) error {
	return api.send(0, 8, []byte{}, func(buf *bytes.Buffer) {
		var info SystemInfo
		binary.Read(buf, binary.LittleEndian, &info)
		completion(&info)
	})
}

// SystemEndpointTx transmit endpoint
func (api *API) SystemEndpointTx(endpoint byte, data []byte, completion func(uint16)) error {
	args := []byte{endpoint, byte(len(data))}
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, args)
	binary.Write(buf, binary.LittleEndian, buf)
	return api.send(0, 9, buf.Bytes(), func(buf *bytes.Buffer) {
		var endpoint uint16
		binary.Read(buf, binary.LittleEndian, endpoint)
		completion(endpoint)
	})
}

// SystemWhitelistAppend append mac to whitelist
func (api *API) SystemWhitelistAppend(address QualifiedMac, completion func(uint16)) error {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, address)
	return api.send(0, 10, buf.Bytes(), func(buf *bytes.Buffer) {
		var result uint16
		binary.Read(buf, binary.LittleEndian, result)
		completion(result)
	})
}

// SystemWhitelistRemove remove mac from whitelist
func (api *API) SystemWhitelistRemove(address QualifiedMac) error {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, address)
	return api.send(0, 11, buf.Bytes(), func(buf *bytes.Buffer) {})
}

// SystemWhitelistClear clear the whitelist
func (api *API) SystemWhitelistClear() error {
	return api.send(0, 12, []byte{}, func(buf *bytes.Buffer) {})
}

// SystemEndpointRx receive whitelist
func (api *API) SystemEndpointRx(endpoint byte, size byte) error {
	return api.send(0, 13, []byte{endpoint, size}, func(buf *bytes.Buffer) {})
}

// SystemEndpointSetWatermarks set watermarks
func (api *API) SystemEndpointSetWatermarks(endpoint byte, rx byte, tx byte) error {
	return api.send(0, 14, []byte{endpoint, rx, tx}, func(buf *bytes.Buffer) {})
}

// FlashPsDefrag defragment flash
func (api *API) FlashPsDefrag() error {
	return api.send(1, 0, []byte{}, func(buf *bytes.Buffer) {})
}

// FlashPsDump dump flash
func (api *API) FlashPsDump() error {
	return api.send(1, 1, []byte{}, func(buf *bytes.Buffer) {})
}

// FlashPsEraseAll erase flash
func (api *API) FlashPsEraseAll() error {
	return api.send(1, 2, []byte{}, func(buf *bytes.Buffer) {})
}

// FlashPsSave save key value pair
func (api *API) FlashPsSave(key uint16, value []byte) error {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, key)
	binary.Write(buf, binary.LittleEndian, byte(len(value)))
	binary.Write(buf, binary.LittleEndian, value)
	return api.send(1, 3, buf.Bytes(), func(buf *bytes.Buffer) {})
}

// FlashPsLoad load key value pair
func (api *API) FlashPsLoad(key uint16) error {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, key)
	return api.send(1, 4, buf.Bytes(), func(buf *bytes.Buffer) {})
}

// FlashPsErase erase key value pair
func (api *API) FlashPsErase(key uint16) error {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, key)
	return api.send(1, 5, buf.Bytes(), func(buf *bytes.Buffer) {})
}

// FlashErasePage erase page
func (api *API) FlashErasePage(page byte) error {
	return api.send(1, 5, []byte{page}, func(buf *bytes.Buffer) {})
}

// FlashWriteWords write words
func (api *API) FlashWriteWords(address uint16, words []byte) error {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, address)
	binary.Write(buf, binary.LittleEndian, byte(len(words)))
	binary.Write(buf, binary.LittleEndian, words)
	return api.send(1, 7, buf.Bytes(), func(buf *bytes.Buffer) {})
}

// AttributesWrite write attributes
func (api *API) AttributesWrite(handle uint16, offset byte, value []byte) error {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, handle)
	binary.Write(buf, binary.LittleEndian, offset)
	binary.Write(buf, binary.LittleEndian, byte(len(value)))
	binary.Write(buf, binary.LittleEndian, value)
	return api.send(2, 0, buf.Bytes(), func(buf *bytes.Buffer) {})
}

// AttributesRead read attributes
func (api *API) AttributesRead(handle uint16, offset byte) error {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, handle)
	binary.Write(buf, binary.LittleEndian, offset)
	return api.send(2, 1, buf.Bytes(), func(buf *bytes.Buffer) {})
}

// AttributesReadType read attributes type
func (api *API) AttributesReadType(handle uint16) error {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, handle)
	return api.send(2, 2, buf.Bytes(), func(buf *bytes.Buffer) {})
}

// AttributesUserReadResponse read user response
func (api *API) AttributesUserReadResponse(connection byte, attError byte, value []byte) error {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, connection)
	binary.Write(buf, binary.LittleEndian, attError)
	binary.Write(buf, binary.LittleEndian, byte(len(value)))
	binary.Write(buf, binary.LittleEndian, value)
	return api.send(2, 3, buf.Bytes(), func(buf *bytes.Buffer) {})
}

// AttributesUserWriteResponse write response
func (api *API) AttributesUserWriteResponse(connection byte, attError byte) error {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, connection)
	binary.Write(buf, binary.LittleEndian, attError)
	return api.send(2, 4, buf.Bytes(), func(buf *bytes.Buffer) {})
}

// ConnectionDisconnect disconnect
func (api *API) ConnectionDisconnect(connection byte) error {
	return api.send(3, 0, []byte{connection}, func(buf *bytes.Buffer) {})
}

// ConnectionGetRssi get the RSSI value
func (api *API) ConnectionGetRssi(connection byte) error {
	return api.send(3, 1, []byte{connection}, func(buf *bytes.Buffer) {})
}

// ConnectionUpdate update connection params
func (api *API) ConnectionUpdate(connection byte, params *ConnectionParameters) error {
	params2 := *params
	// FIXME confirm that these are really swapped
	params2.Latency = params.Timeout
	params2.Timeout = params.Latency
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, connection)
	binary.Write(buf, binary.LittleEndian, params2)

	return api.send(3, 2, buf.Bytes(), func(buf *bytes.Buffer) {})
}

// ConnectionVersionUpdate update version
func (api *API) ConnectionVersionUpdate(connection byte) error {
	return api.send(3, 3, []byte{connection}, func(buf *bytes.Buffer) {})
}

// ConnectionChannelMapGet get channel mapping
func (api *API) ConnectionChannelMapGet(connection byte) error {
	return api.send(3, 4, []byte{connection}, func(buf *bytes.Buffer) {})
}

// ConnectionChannelMapSet set channel mapping
func (api *API) ConnectionChannelMapSet(connection byte, connMap []byte) error {
	return api.send(3, 5, append([]byte{connection, byte(len(connMap))}, connMap...), func(buf *bytes.Buffer) {})
}

// ConnectionFeaturesGet get connection features
func (api *API) ConnectionFeaturesGet(connection byte) error {
	return api.send(3, 6, []byte{connection}, func(buf *bytes.Buffer) {})
}

// ConnectionStatusGet get connection status
func (api *API) ConnectionStatusGet(connection byte) error {
	return api.send(3, 7, []byte{connection}, func(buf *bytes.Buffer) {})
}

// ConnectionRawTx transmit raw data
func (api *API) ConnectionRawTx(connection byte, data []byte) error {
	return api.send(3, 8, append([]byte{connection, byte(len(data))}, data...), func(buf *bytes.Buffer) {})
}

// AttclientFindByTypeValue find attribute client by type
func (api *API) AttclientFindByTypeValue(connection byte, start uint16, end uint16, uuid uint16, value []byte) error {
	data := struct {
		connection byte
		start      uint16
		end        uint16
		uuid       uint16
		valueLen   byte
		value      []byte
	}{
		connection,
		start,
		end,
		uuid,
		byte(len(value)),
		value,
	}
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, data)
	return api.send(4, 0, buf.Bytes(), func(buf *bytes.Buffer) {})
}

// AttclientReadByGroupType query for discovered services
// NOTE: Discovered services are reported by OnAttrclientGroupFound
func (api *API) AttclientReadByGroupType(connection byte, start uint16, end uint16, uuid []byte) error {
	data := struct {
		connection byte
		start      uint16
		end        uint16
		uuidLen    byte
		uuid       []byte
	}{
		connection,
		start,
		end,
		byte(len(uuid)),
		uuid,
	}
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, data)
	return api.send(4, 1, buf.Bytes(), func(buf *bytes.Buffer) {})
}

// AttclientReadByType read by group type
func (api *API) AttclientReadByType(connection byte, start uint16, end uint16, uuid []byte) error {
	data := struct {
		connection byte
		start      uint16
		end        uint16
		uuidLen    byte
		uuid       []byte
	}{
		connection,
		start,
		end,
		byte(len(uuid)),
		uuid,
	}
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, data)
	return api.send(4, 2, buf.Bytes(), func(buf *bytes.Buffer) {})
}

// AttclientFindInformation find information
func (api *API) AttclientFindInformation(connection byte, start uint16, end uint16) error {
	data := struct {
		connection byte
		start      uint16
		end        uint16
	}{
		connection,
		start,
		end,
	}
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, data)
	return api.send(4, 3, buf.Bytes(), func(buf *bytes.Buffer) {})
}

// AttclientReadByHandle read by characteristic handle
func (api *API) AttclientReadByHandle(connection byte, handle uint16) error {
	data := struct {
		connection byte
		handle     uint16
	}{
		connection,
		handle,
	}
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, data)
	return api.send(4, 4, buf.Bytes(), func(buf *bytes.Buffer) {})
}

// AttclientAttributeWrite write to an attribute
func (api *API) AttclientAttributeWrite(connection byte, handle uint16, data []uint8) error {
	toSend := struct {
		connection byte
		handle     uint16
		dataLen    byte
		data       []byte
	}{
		connection,
		handle,
		byte(len(data)),
		data,
	}
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, toSend)
	return api.send(4, 5, buf.Bytes(), func(buf *bytes.Buffer) {})
}

// AttclientWriteCommand write command data
func (api *API) AttclientWriteCommand(connection byte, handle uint16, data []uint8) error {
	toSend := struct {
		connection byte
		handle     uint16
		dataLen    byte
		data       []byte
	}{
		connection,
		handle,
		byte(len(data)),
		data,
	}
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, toSend)
	return api.send(4, 6, buf.Bytes(), func(buf *bytes.Buffer) {})
}

// AttrclientIndicateConfirm confirm indication
func (api *API) AttrclientIndicateConfirm(connection byte) error {
	return api.send(4, 7, []byte{connection}, func(buf *bytes.Buffer) {})
}

// AttclientReadLong iniiate a long read
func (api *API) AttclientReadLong(connection byte, handle uint16) error {
	data := struct {
		connection byte
		handle     uint16
	}{
		connection,
		handle,
	}
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, data)
	return api.send(4, 8, buf.Bytes(), func(buf *bytes.Buffer) {})
}

// AttclientPrepareWrite prepare to write
func (api *API) AttclientPrepareWrite(connection byte, handle uint16, offset uint16, data []byte) error {
	toSend := struct {
		connection byte
		handle     uint16
		offset     uint16
		dataLen    byte
		data       []byte
	}{
		connection,
		handle,
		offset,
		byte(len(data)),
		data,
	}
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, toSend)
	return api.send(4, 9, buf.Bytes(), func(buf *bytes.Buffer) {})
}

// AttrclientExecuteWrite execute write
func (api *API) AttrclientExecuteWrite(connection byte, commit byte) error {
	return api.send(4, 10, []byte{commit}, func(buf *bytes.Buffer) {})
}

// AttrclientReadMultiple read multiple handles (FIXME should it be uint16)
func (api *API) AttrclientReadMultiple(connection byte, handles []byte) error {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, connection)
	binary.Write(buf, binary.LittleEndian, byte(len(handles)))
	binary.Write(buf, binary.LittleEndian, handles)
	return api.send(4, 11, buf.Bytes(), func(buf *bytes.Buffer) {})
}

// SmEncryptStart start encryption
func (api *API) SmEncryptStart(handle byte, bonding byte) error {
	return api.send(5, 0, []byte{handle, bonding}, func(buf *bytes.Buffer) {})
}

// SmSetBondableMode set bondable mode
func (api *API) SmSetBondableMode(bondable byte) error {
	return api.send(5, 1, []byte{bondable}, func(buf *bytes.Buffer) {})
}

// SmDeleteBonding delete bonding
func (api *API) SmDeleteBonding(handle byte) error {
	return api.send(5, 2, []byte{handle}, func(buf *bytes.Buffer) {})
}

// SmSetParameters set security parameters
func (api *API) SmSetParameters(mitm byte, minKeySize byte, ioCapabilities byte) error {
	return api.send(5, 3, []byte{mitm, minKeySize, ioCapabilities}, func(buf *bytes.Buffer) {})
}

// SmPasskeyEntry set security passkey
func (api *API) SmPasskeyEntry(handle byte, passkey uint32) error {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, passkey)
	return api.send(5, 4, buf.Bytes(), func(buf *bytes.Buffer) {})
}

// SmGetBonds get bonding
func (api *API) SmGetBonds() error {
	return api.send(5, 5, []byte{}, func(buf *bytes.Buffer) {})
}

// SmSetOobData set oob data
func (api *API) SmSetOobData(oob []byte) error {
	data := append([]byte{byte(len(oob))}, oob...)
	return api.send(5, 6, data, func(buf *bytes.Buffer) {})
}

// GapSetPrivacyFlags set GAP privacy flags
func (api *API) GapSetPrivacyFlags(periphPrivacy byte, centralPrivacy byte) error {
	return api.send(6, 0, []byte{periphPrivacy, centralPrivacy}, func(buf *bytes.Buffer) {})
}

// GapSetMode set GAP mode
func (api *API) GapSetMode(discover byte, connect byte) error {
	return api.send(6, 1, []byte{discover, connect}, func(buf *bytes.Buffer) {})
}

// GapDiscover set GAP discovery mode
func (api *API) GapDiscover(mode byte) error {
	return api.send(6, 2, []byte{mode}, func(buf *bytes.Buffer) {})
}

// GapConnectDirect set GAP connection parameters for directed discovery
func (api *API) GapConnectDirect(mac QualifiedMac, params *ConnectionParameters) error {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, mac.Address)
	binary.Write(buf, binary.LittleEndian, mac.AddrType)
	binary.Write(buf, binary.LittleEndian, params)
	return api.send(6, 3, buf.Bytes(), func(buf *bytes.Buffer) {})
}

// GapEndProcedure end GAP procedure
func (api *API) GapEndProcedure() error {
	return api.send(6, 4, []byte{}, func(buf *bytes.Buffer) {})
}

// GapConnectSelective set GAP connetion paramters for selective discovery
func (api *API) GapConnectSelective(params *ConnectionParameters) error {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, params)
	return api.send(6, 5, buf.Bytes(), func(buf *bytes.Buffer) {})
}

// GapSetFiltering set GAP filtering policy
func (api *API) GapSetFiltering(scanPolicy byte, advPolicy byte, scanDuplicateFiltering byte) error {
	return api.send(6, 6, []byte{scanPolicy, advPolicy, scanDuplicateFiltering}, func(buf *bytes.Buffer) {})
}

// GapSetScanParameters set GAP scanning parameters
func (api *API) GapSetScanParameters(scanInterval uint16, scanWindow uint16, active byte) error {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, scanInterval)
	binary.Write(buf, binary.LittleEndian, scanWindow)
	binary.Write(buf, binary.LittleEndian, active)
	return api.send(6, 7, buf.Bytes(), func(buf *bytes.Buffer) {})
}

// GapSetAdvParameters set GAP advertisement parameters
func (api *API) GapSetAdvParameters(intervalMin uint16, intervalMax uint16, channels uint8) error {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, intervalMin)
	binary.Write(buf, binary.LittleEndian, intervalMax)
	binary.Write(buf, binary.LittleEndian, channels)
	return api.send(6, 8, buf.Bytes(), func(buf *bytes.Buffer) {})
}

// GapSetAdvData set GAP advertisement data
func (api *API) GapSetAdvData(setScanResp byte, advData []byte) error {
	data := append([]byte{setScanResp, byte(len(advData))}, advData...)
	return api.send(6, 9, data, func(buf *bytes.Buffer) {})
}

// GapSetDirectedConnectableMode set directed connectable mode
func (api *API) GapSetDirectedConnectableMode(address []byte, addrType byte) error {
	data := append(address, []byte{addrType}...)
	return api.send(6, 10, data, func(buf *bytes.Buffer) {})
}

// HardwareIoPortConfigIrq configure the port's IRQ
func (api *API) HardwareIoPortConfigIrq(port byte, enableBits byte, fallingEdge byte) error {
	return api.send(7, 0, []byte{port, enableBits, fallingEdge}, func(buf *bytes.Buffer) {})
}

// HardwareSetSoftTimer configure the soft timer
func (api *API) HardwareSetSoftTimer(time uint32, handle byte, singleShot byte) error {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, time)
	binary.Write(buf, binary.LittleEndian, handle)
	binary.Write(buf, binary.LittleEndian, singleShot)
	return api.send(7, 1, buf.Bytes(), func(buf *bytes.Buffer) {})
}

// HardwareAdcRead read the ADC value
func (api *API) HardwareAdcRead(input byte, decimation byte, refrenceSelection byte) error {
	return api.send(7, 2, []byte{input, decimation, refrenceSelection}, func(buf *bytes.Buffer) {})
}

// HardwareIoPortConfgDirection configure the IO's direction
func (api *API) HardwareIoPortConfgDirection(port byte, direction byte) error {
	return api.send(7, 3, []byte{port, direction}, func(buf *bytes.Buffer) {})
}

// HardwareIoPortConfigFunction configure the IO's function
func (api *API) HardwareIoPortConfigFunction(port byte, function byte) error {
	return api.send(7, 4, []byte{port, function}, func(buf *bytes.Buffer) {})
}

// HardwareIoPortConfigPull configure the port as pullUp
func (api *API) HardwareIoPortConfigPull(port byte, triStateMask byte, pullUp byte) error {
	return api.send(7, 5, []byte{port, triStateMask, pullUp}, func(buf *bytes.Buffer) {})
}

// HardwareIoPortWrite write to IO
func (api *API) HardwareIoPortWrite(port byte, mask byte, data byte) error {
	return api.send(7, 6, []byte{port, mask, data}, func(buf *bytes.Buffer) {})
}

// HardwareIoPortRead read from IO
func (api *API) HardwareIoPortRead(port byte, mask byte) error {
	return api.send(7, 7, []byte{port, mask}, func(buf *bytes.Buffer) {})
}

// HardwareSpiConfig configure SPI
func (api *API) HardwareSpiConfig(channel byte, config *SpiConfig) error {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, channel)
	binary.Write(buf, binary.LittleEndian, config)
	return api.send(7, 8, buf.Bytes(), func(buf *bytes.Buffer) {})
}

// HardwareSpiTx SPI transmit
func (api *API) HardwareSpiTx(channel byte, data []byte) error {
	toSend := append([]byte{channel, byte(len(data))}, data...)
	return api.send(7, 9, toSend, func(buf *bytes.Buffer) {})
}

// HardwareI2cRead read I2C device
func (api *API) HardwareI2cRead(address byte, stop byte, length byte) error {
	return api.send(7, 10, []byte{address, stop, length}, func(buf *bytes.Buffer) {})
}

// HardwareI2cWrite write I2C device
func (api *API) HardwareI2cWrite(address byte, stop byte, data []byte) error {
	toSend := append([]byte{address, stop, byte(len(data))}, data...)
	return api.send(7, 11, toSend, func(buf *bytes.Buffer) {})
}

// HardwareI2cSetTxPower set I2C transmit power
func (api *API) HardwareI2cSetTxPower(power byte) error {
	return api.send(7, 12, []byte{power}, func(buf *bytes.Buffer) {})
}

// HardwareTimerComparitor configure the hardware timer comparitor
func (api *API) HardwareTimerComparitor(timer byte, channel byte, mode byte, comparitorValue uint16) error {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, timer)
	binary.Write(buf, binary.LittleEndian, channel)
	binary.Write(buf, binary.LittleEndian, mode)
	binary.Write(buf, binary.LittleEndian, comparitorValue)
	return api.send(7, 13, buf.Bytes(), func(buf *bytes.Buffer) {})
}

// TestPhyTx test transmiter
func (api *API) TestPhyTx(channel byte, length byte, testType byte) error {
	return api.send(8, 0, []byte{channel, length, testType}, func(buf *bytes.Buffer) {})
}

// TestPhyRx test receiver
func (api *API) TestPhyRx(channel byte) error {
	return api.send(8, 1, []byte{channel}, func(buf *bytes.Buffer) {})
}

// TestPhyEnd test end
func (api *API) TestPhyEnd() error {
	return api.send(8, 2, []byte{}, func(buf *bytes.Buffer) {})
}

// TestPhyReset test reset
func (api *API) TestPhyReset() error {
	return api.send(8, 3, []byte{}, func(buf *bytes.Buffer) {})
}

// TestGetChannelMap test get channel map
func (api *API) TestGetChannelMap() error {
	return api.send(8, 4, []byte{}, func(buf *bytes.Buffer) {})
}

// TestDebug loopback?
func (api *API) TestDebug(data []byte) error {
	toSend := append([]byte{byte(len(data))}, data...)
	return api.send(8, 5, toSend, func(buf *bytes.Buffer) {})
}

//
// delegate methods
//

// OnSystemBoot invoked when the BLED112 boots
func (dgt *LoggingDelegate) OnSystemBoot(info *SystemInfo) {

}

// OnSystemDebug invoked when BLED112 generates debug reply
func (dgt *LoggingDelegate) OnSystemDebug(data []byte) {

}

// OnSystemEndpointWatermarkRx inovked when receiveing Endpoint Watermark
func (dgt *LoggingDelegate) OnSystemEndpointWatermarkRx(endpoint byte, data byte) {

}

// OnSystemEndpointWatermarkTx inovked when transmitting Endpoint Watermark
func (dgt *LoggingDelegate) OnSystemEndpointWatermarkTx(endpoint byte, data byte) {

}

// OnSystemScriptFailure invoked on script failure
func (dgt *LoggingDelegate) OnSystemScriptFailure(addr uint16, reason uint16) {

}

// OnSystemNoLicenseKey invoked when no license key is found
func (dgt *LoggingDelegate) OnSystemNoLicenseKey() {

}

// OnFlashPsKey invoked when flash PS Key is updated
func (dgt *LoggingDelegate) OnFlashPsKey(key uint16, value []byte) {

}

// OnAttributeValue invoked when attribute value changes
func (dgt *LoggingDelegate) OnAttributeValue(connection byte, reason byte, handle uint16, offset uint16, value []byte) {

}

// OnAttributeUserReadRequest inovked by user read request
func (dgt *LoggingDelegate) OnAttributeUserReadRequest(connection byte, handle uint16, offset uint16, maxSize byte) {

}

// OnAttributeStatus invoked when status changes
func (dgt *LoggingDelegate) OnAttributeStatus(handle uint16, flags byte) {

}

// OnConnectionStatus invoked when the connection status changes
func (dgt *LoggingDelegate) OnConnectionStatus(status *ConnectionStatus) {
}

// OnConnectionVersionIndication invoked when version indication is updated
func (dgt *LoggingDelegate) OnConnectionVersionIndication(ind *ConnectionVersionIndication) {
}

// OnConnectionFeatureIndication invoked when feature indication is updated
func (dgt *LoggingDelegate) OnConnectionFeatureIndication(connection byte, features []byte) {
}

// OnConnectionRawRx invoked when raw data is received
func (dgt *LoggingDelegate) OnConnectionRawRx(connection byte, data []byte) {
}

// OnConnectionDisconnected invoked when the connection is lost
func (dgt *LoggingDelegate) OnConnectionDisconnected(connection byte, reason uint16) {
}

// OnAttrclientIndicated inovked when an attribute is indicated
func (dgt *LoggingDelegate) OnAttrclientIndicated(connection byte, attrHandle uint16) {
}

// OnAttrclientProcedureCompleted invoked upon procedure completion
func (dgt *LoggingDelegate) OnAttrclientProcedureCompleted(connection byte, result uint16, chrHandle uint16) {
}

// OnAttrclientGroupFound invoked when the group is found
func (dgt *LoggingDelegate) OnAttrclientGroupFound(connection byte, start uint16, end uint16, uuid []byte) {
}

// OnAttrclientAttributeFound invoked when the attribute is found
func (dgt *LoggingDelegate) OnAttrclientAttributeFound(connection byte, chrdecl uint16, value uint16, properties byte, uuid []byte) {
}

// OnAttrclientFindInformationFound invoked when information is available
func (dgt *LoggingDelegate) OnAttrclientFindInformationFound(connection byte, chrHandle uint16, uuid []byte) {
}

// OnAttrclientAttributeValue invoked when value changes
func (dgt *LoggingDelegate) OnAttrclientAttributeValue(connection byte, attHandle uint16, valueType byte, value []byte) {
}

// OnAttrclientReadMultipleResponse invoked when the client responds
func (dgt *LoggingDelegate) OnAttrclientReadMultipleResponse(connection byte, handles []byte) {}

// OnGapScanResponse invoked when GAP Scan Response is available
func (dgt *LoggingDelegate) OnGapScanResponse(resp *GapScanRespone) {
}

// OnGapModeChanged invoked when the GAP mode changes
func (dgt *LoggingDelegate) OnGapModeChanged(discover byte, connect byte) {

}

// OnSmSmpData invoked when security manager data is posted
func (dgt *LoggingDelegate) OnSmSmpData(handle byte, packet byte, data []byte) {}

// OnSmBondingFail invoked when the bonding fails
func (dgt *LoggingDelegate) OnSmBondingFail(handle byte, result uint16) {}

// OnSmPasskeyDisplay inovked when the paskey is displayed
func (dgt *LoggingDelegate) OnSmPasskeyDisplay(handle byte, passkey uint32) {}

// OnSmPasskeyRequest invoked when the paskey is requested
func (dgt *LoggingDelegate) OnSmPasskeyRequest(handle byte) {}

// OnSmBondStatus invoked when the bond status is updated
func (dgt *LoggingDelegate) OnSmBondStatus(status *SmBondStatus) {}

// OnHardwareIoPortStatus invoked when the IO port status is changed
func (dgt *LoggingDelegate) OnHardwareIoPortStatus(status *IoPortStatus) {}

// OnHardwareSoftTimer invoked upon soft timer expiry
func (dgt *LoggingDelegate) OnHardwareSoftTimer(handle byte) {}

// OnHardwareAdcResult invoked when ADC result is generated
func (dgt *LoggingDelegate) OnHardwareAdcResult(input byte, value int16) {}

//
// event parser
//

func (api *API) parseSystemEvent(cmdType byte, buf *bytes.Buffer) {
	switch cmdType {
	case 0:
		var info SystemInfo
		binary.Read(buf, binary.LittleEndian, &info)
		api.delegate.OnSystemBoot(&info)
	case 1:
		buf.ReadByte() // skip length
		api.delegate.OnSystemDebug(buf.Bytes())
	case 2:
		endpoint, _ := buf.ReadByte()
		data, _ := buf.ReadByte()
		api.delegate.OnSystemEndpointWatermarkRx(endpoint, data)
	case 3:
		endpoint, _ := buf.ReadByte()
		data, _ := buf.ReadByte()
		api.delegate.OnSystemEndpointWatermarkTx(endpoint, data)
	case 4:
		var addr uint16
		var value uint16
		binary.Read(buf, binary.LittleEndian, &addr)
		binary.Read(buf, binary.LittleEndian, &value)
		api.delegate.OnSystemScriptFailure(addr, value)
	case 5:
		api.delegate.OnSystemNoLicenseKey()
	}
}

func (api *API) parseFlashPsEvent(cmdType byte, buf *bytes.Buffer) {
	if cmdType != 0 {
		return
	}

	var key uint16
	binary.Read(buf, binary.LittleEndian, &key)
	buf.ReadByte() // skip length
	api.delegate.OnFlashPsKey(key, buf.Bytes())
}

func (api *API) parseAttributeEvent(cmdType byte, buf *bytes.Buffer) {
	switch cmdType {
	case 0:
		var connection, reason byte
		var handle, offset uint16
		binary.Read(buf, binary.LittleEndian, &connection)
		binary.Read(buf, binary.LittleEndian, &reason)
		binary.Read(buf, binary.LittleEndian, &handle)
		binary.Read(buf, binary.LittleEndian, &offset)
		buf.ReadByte() // skip length
		api.delegate.OnAttributeValue(connection, reason, handle, offset, buf.Bytes())
	case 1:
		var connection, maxSize byte
		var handle, offset uint16
		binary.Read(buf, binary.LittleEndian, &connection)
		binary.Read(buf, binary.LittleEndian, &handle)
		binary.Read(buf, binary.LittleEndian, &offset)
		binary.Read(buf, binary.LittleEndian, &maxSize)
		api.delegate.OnAttributeUserReadRequest(connection, handle, offset, maxSize)
	case 2:
		var handle uint16
		var flags byte
		api.delegate.OnAttributeStatus(handle, flags)
	}
}

func (api *API) parseConnectionEvent(cmdType byte, buf *bytes.Buffer) {
	switch cmdType {
	case 0:
		var status ConnectionStatus
		binary.Read(buf, binary.LittleEndian, &status)
		api.delegate.OnConnectionStatus(&status)
	case 1:
		var ind ConnectionVersionIndication
		binary.Read(buf, binary.LittleEndian, &ind)
		api.delegate.OnConnectionVersionIndication(&ind)
	case 2:
		var connection, featureLen byte
		binary.Read(buf, binary.LittleEndian, &connection)
		binary.Read(buf, binary.LittleEndian, &featureLen)
		api.delegate.OnConnectionFeatureIndication(connection, buf.Bytes()[:featureLen])
	case 3:
		var connection, dataLen byte
		binary.Read(buf, binary.LittleEndian, &connection)
		binary.Read(buf, binary.LittleEndian, &dataLen)
		api.delegate.OnConnectionRawRx(connection, buf.Bytes()[:dataLen])
	case 4:
		var connection byte
		var reason uint16
		api.delegate.OnConnectionDisconnected(connection, reason)
	}
}

func (api *API) parseAttrclientEvent(cmdType byte, buf *bytes.Buffer) {
	if cmdType > 6 {
		return
	}

	var connection byte
	binary.Read(buf, binary.LittleEndian, &connection)

	switch cmdType {
	case 0:
		var attrHandle uint16
		binary.Read(buf, binary.LittleEndian, &attrHandle)
		api.delegate.OnAttrclientIndicated(connection, attrHandle)
	case 1:
		var result, chrHandle uint16
		binary.Read(buf, binary.LittleEndian, &result)
		binary.Read(buf, binary.LittleEndian, &chrHandle)
		api.delegate.OnAttrclientProcedureCompleted(connection, result, chrHandle)
	case 2:
		var start, end uint16
		var uuidLen byte
		binary.Read(buf, binary.LittleEndian, &start)
		binary.Read(buf, binary.LittleEndian, &end)
		binary.Read(buf, binary.LittleEndian, &uuidLen)
		api.delegate.OnAttrclientGroupFound(connection, start, end, buf.Bytes()[:uuidLen])
	case 3:
		var chrdecl, value uint16
		var properties, uuidLen byte
		binary.Read(buf, binary.LittleEndian, &chrdecl)
		binary.Read(buf, binary.LittleEndian, &value)
		binary.Read(buf, binary.LittleEndian, &properties)
		binary.Read(buf, binary.LittleEndian, &uuidLen)
		api.delegate.OnAttrclientAttributeFound(connection, chrdecl, value, properties, buf.Bytes()[:uuidLen])
	case 4:
		var chrHandle uint16
		var uuidLen byte
		binary.Read(buf, binary.LittleEndian, &chrHandle)
		binary.Read(buf, binary.LittleEndian, &uuidLen)
		api.delegate.OnAttrclientFindInformationFound(connection, chrHandle, buf.Bytes()[:uuidLen])
	case 5:
		var attHandle uint16
		var valueType, valueLen byte
		binary.Read(buf, binary.LittleEndian, &attHandle)
		binary.Read(buf, binary.LittleEndian, &valueType)
		binary.Read(buf, binary.LittleEndian, &valueLen)
		api.delegate.OnAttrclientAttributeValue(connection, attHandle, valueType, buf.Bytes()[:valueLen])
	case 6:
		var handlesLen byte
		binary.Read(buf, binary.LittleEndian, &handlesLen)
		api.delegate.OnAttrclientReadMultipleResponse(connection, buf.Bytes()[:handlesLen])
	}
}

func (api *API) parseSmEvent(cmdType byte, buf *bytes.Buffer) {
	if cmdType == 4 {
		// special case where there is no handle in command
		var status SmBondStatus
		binary.Read(buf, binary.LittleEndian, &status)
		api.delegate.OnSmBondStatus(&status)
		return
	} else if cmdType > 4 {
		return
	}

	var handle byte
	binary.Read(buf, binary.LittleEndian, &handle)

	switch cmdType {
	case 0:
		packet, _ := buf.ReadByte()
		dataLen, _ := buf.ReadByte()
		api.delegate.OnSmSmpData(handle, packet, buf.Bytes()[:dataLen])
	case 1:
		var result uint16
		binary.Read(buf, binary.LittleEndian, &result)
		api.delegate.OnSmBondingFail(handle, result)
	case 2:
		var passkey uint32
		binary.Read(buf, binary.LittleEndian, &passkey)
		api.delegate.OnSmPasskeyDisplay(handle, passkey)
	case 3:
		api.delegate.OnSmPasskeyRequest(handle)
	}
}

func (api *API) parseGapEvent(cmdType byte, buf *bytes.Buffer) {
	switch cmdType {
	case 0:
		var resp GapScanRespone
		binary.Read(buf, binary.LittleEndian, &resp.RSSI)
		binary.Read(buf, binary.LittleEndian, &resp.PacketType)
		binary.Read(buf, binary.LittleEndian, &resp.Address)
		binary.Read(buf, binary.LittleEndian, &resp.Bond)
		resp.Data = buf.Bytes()
		api.delegate.OnGapScanResponse(&resp)
	case 1:
		var discover, connect byte
		binary.Read(buf, binary.LittleEndian, &discover)
		binary.Read(buf, binary.LittleEndian, &connect)
		api.delegate.OnGapModeChanged(discover, connect)
	}
}

func (api *API) parseHardwareEvent(cmdType byte, buf *bytes.Buffer) {
	switch cmdType {
	case 0:
		var status IoPortStatus
		binary.Read(buf, binary.LittleEndian, &status)
		api.delegate.OnHardwareIoPortStatus(&status)
	case 1:
		var handle byte
		binary.Read(buf, binary.LittleEndian, &handle)
		api.delegate.OnHardwareSoftTimer(handle)
	case 2:
		var input byte
		var value int16
		api.delegate.OnHardwareAdcResult(input, value)
	}
}

func (api *API) parseEvent(hdr *bgFrameHeader, buf *bytes.Buffer) {
	switch hdr.packetClass {
	case 0:
		api.parseSystemEvent(hdr.packetCommand, buf)
	case 1:
		api.parseFlashPsEvent(hdr.packetCommand, buf)
	case 2:
		api.parseAttributeEvent(hdr.packetCommand, buf)
	case 3:
		api.parseConnectionEvent(hdr.packetCommand, buf)
	case 4:
		api.parseAttrclientEvent(hdr.packetCommand, buf)
	case 5:
		api.parseSmEvent(hdr.packetCommand, buf)
	case 6:
		api.parseGapEvent(hdr.packetCommand, buf)
	case 7:
		api.parseHardwareEvent(hdr.packetCommand, buf)
	}
}
