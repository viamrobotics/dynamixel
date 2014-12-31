package dynamixel

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"strings"
)

const (

	// Instruction Types
	Ping      byte = 0x01
	ReadData  byte = 0x02
	WriteData byte = 0x03
	RegWrite  byte = 0x04
	Action    byte = 0x05
	Reset     byte = 0x06
	SyncWrite byte = 0x83

	// Send an instruction to all servos
	BroadcastIdent byte = 0xFE // 254
)

type DynamixelNetwork struct {
	Serial   io.ReadWriteCloser
	Buffered bool
	Debug    bool
}

func NewNetwork(serial io.ReadWriteCloser) *DynamixelNetwork {
	return &DynamixelNetwork{
		Serial:   serial,
		Buffered: false,
		Debug:    false,
	}
}

//
// Puts the network in bufferred write mode, which means that the REG_WRITE
// instruction will be used, rather than WRITE_DATA. This causes calls to
// WriteData to be bufferred until the Action method is called, at which time
// they'll all be executed at once.
//
// This is very useful for synchronizing the movements of multiple servos.
//
func (n *DynamixelNetwork) SetBuffered(buffered bool) {
	n.Buffered = buffered
}

//
// Converts an error byte (as included in a status packet) into an error object
// with a friendly error message. We can't be too specific about it, because any
// combination of errors might occur at the same time.
//
// See: http://support.robotis.com/en/product/dynamixel/communication/dxl_packet.htm#Status_Packet
//
func DecodeStatusError(errBits byte) error {
	str := []string{}

	if errBits&1 == 1 {
		str = append(str, "input voltage")
	}

	if errBits&2 == 2 {
		str = append(str, "angle limit")
	}

	if errBits&4 == 4 {
		str = append(str, "overheating")
	}

	if errBits&8 == 8 {
		str = append(str, "range")
	}

	if errBits&16 == 16 {
		str = append(str, "checksum")
	}

	if errBits&32 == 32 {
		str = append(str, "overload")
	}

	if errBits&64 == 64 {
		str = append(str, "instruction")
	}

	if errBits&128 == 128 {
		str = append(str, "unknown")
	}

	return fmt.Errorf("status error(s): %s", strings.Join(str, ", "))
}

//
// This stuff is generic to all Dynamixels. See:
//
// * http://support.robotis.com/en/product/dynamixel/communication/dxl_packet.htm
// * http://support.robotis.com/en/product/dynamixel/communication/dxl_instruction.htm
//
func (network *DynamixelNetwork) WriteInstruction(ident uint8, instruction byte, params ...byte) error {
	buf := new(bytes.Buffer)
	paramsLength := byte(len(params) + 2)

	// build instruction packet

	buf.Write([]byte{
		0xFF,               // header
		0xFF,               // header
		byte(ident),        // target Dynamixel ID
		byte(paramsLength), // len(params) + 2
		instruction,        // instruction type (read/write/etc)
	})

	buf.Write(params)

	// calculate checksum

	sum := ident + paramsLength + instruction

	for _, value := range params {
		sum += value
	}

	buf.WriteByte(byte((^sum) & 0xFF))

	// write to port

	network.Log(">> %#v\n", buf.Bytes())
	_, err := buf.WriteTo(network.Serial)

	if err != nil {
		return err
	}

	return nil
}

//
// Reads `b` bytes, blocking if they're not immediately available.
//
// TODO(adammck): Add a timeout parameter, so instructions which we know might
//                fail (e.g. PING) can do so gracefully, rather than hanging.
//
func (network *DynamixelNetwork) read(b int) ([]byte, error) {
	buf := make([]byte, b)
	n := 0

	for n < b {
		nn, err := network.Serial.Read(buf[n:])
		n += nn

		// It's okay if we reached the end of the available bytes. They're probably
		// just not available yet. Other errors are fatal.
		if err != nil && err != io.EOF {
			return buf, err
		}
	}

	return buf, nil
}

func (network *DynamixelNetwork) ReadStatusPacket(expectIdent uint8) ([]byte, error) {

	//
	// Status packets are similar to instruction packet:
	//
	// +------+------+ ------ +-------+----------+---------+--------+--------+----------+
	// | 0xFF | 0xFF |  0xFF  | ident | params+2 | errBits | param1 | param2 | checksum |
	// +------+------+ ------ +-------+----------+---------+--------+--------+----------+
	//
	//                  ^-- sometimes, but it shouldn't be there?!
	//
	// We don't know the length, because responses can have variable numbers of
	// parameters.
	//

	// Read the first three bytes, which are always present. Hopefully, the first
	// two are the header, and the third is the ID of the servo which this packet
	// refers to. But sometimes, the third byte is another 0xFF. I don't know why,
	// and I can't seem to find any useful information on the matter.

	headerBuf, headerErr := network.read(3)
	network.Log("<< %#v (header, ident)\n", headerBuf)
	if headerErr != nil {
		return []byte{}, headerErr
	}

	if headerBuf[0] != 0xFF || headerBuf[1] != 0xFF {
		return []byte{}, fmt.Errorf("bad status packet header: %x %x", headerBuf[0], headerBuf[1])
	}

	// The third byte should be the ident. But if an extra header byte has shown
	// up, ignore it and read another byte to replace it.

	resIdent := uint8(headerBuf[2])
	if resIdent == 255 {

		identBuf, identErr := network.read(1)
		network.Log("<< %#v (ident retry)\n", identBuf)
		if identErr != nil {
			return []byte{}, identErr
		}

		resIdent = uint8(identBuf[0])
	}

	// The next two bytes are always present, so just read them.

	paramCountAndErrBitsBuf, pcebErr := network.read(2)
	network.Log("<< %#v (p+2, errbits)\n", paramCountAndErrBitsBuf)
	if pcebErr != nil {
		return []byte{}, pcebErr
	}

	numParams := uint8(paramCountAndErrBitsBuf[0]) - 2
	errBits := paramCountAndErrBitsBuf[1]
	paramsBuf := make([]byte, numParams)

	// now read the params, if there are any. we must do this before checking for
	// errors, to avoid leaving junk in the buffer.

	if numParams > 0 {
		var paramsErr error
		paramsBuf, paramsErr = network.read(int(numParams))
		network.Log("<< %#v (params)\n", paramsBuf)
		if paramsErr != nil {
			return []byte{}, paramsErr
		}
	}

	// read the checksum, which is always one byte
	// TODO: check the checksum

	checksumBuf, checksumErr := network.read(1)
	network.Log("<< %#v (checksum)\n", checksumBuf)
	if checksumErr != nil {
		return []byte{}, checksumErr
	}

	// return an error if the packet contained one.
	// TODO: decode the error bit(s) and return a proper message!

	if errBits != 0x0 {
		return []byte{}, DecodeStatusError(errBits)
	}

	// return an error if we received a packet with the wrong ID. this indicates
	// a concurrency issue (maybe clashing IDs on a single bus).

	if resIdent != expectIdent {
		return []byte{}, fmt.Errorf("expected status packet for %v, but got %v", expectIdent, resIdent)
	}

	// omg, nothing went wrong

	return paramsBuf, nil
}

// Ping sends the PING instruction to the given Servo ID, and waits for the
// response. Returns an error if the ping fails, or nil if it succeeds.
func (n *DynamixelNetwork) Ping(ident uint8) error {
	n.Log("Ping(%d)", ident)

	writeErr := n.WriteInstruction(ident, Ping)
	if writeErr != nil {
		return writeErr
	}

	// There's no way to disable the status packet for PING commands, so always
	// wait for it. That's how we know that the servo is responding.
	_, readErr := n.ReadStatusPacket(ident)
	if readErr != nil {
		return readErr
	}

	return nil
}

func (n *DynamixelNetwork) ReadData(ident uint8, startAddress byte, length int) (uint16, error) {
	params := []byte{startAddress, byte(length)}
	err1 := n.WriteInstruction(ident, ReadData, params...)
	if err1 != nil {
		return 0, err1
	}

	buf, err2 := n.ReadStatusPacket(ident)
	if err2 != nil {
		return 0, err2
	}

	var val uint16
	err3 := binary.Read(bytes.NewReader(buf), binary.LittleEndian, &val)
	if err3 != nil {
		return 0, err3
	}

	return val, nil
}

func (n *DynamixelNetwork) WriteData(ident uint8, expectStausPacket bool, params ...byte) error {
	var instruction byte

	if n.Buffered {
		instruction = RegWrite
	} else {
		instruction = WriteData
	}

	writeErr := n.WriteInstruction(ident, instruction, params...)
	if writeErr != nil {
		return writeErr
	}

	if expectStausPacket {
		_, readErr := n.ReadStatusPacket(ident)
		if readErr != nil {
			return readErr
		}
	}

	return nil
}

//
// Broadcasts the ACTION instruction, which initiates any previously bufferred
// instructions.
//
// Doesn't wait for a status packet in response, because they are not sent in
// response to broadcast instructions.
//
func (n *DynamixelNetwork) Action() error {
	return n.WriteInstruction(BroadcastIdent, Action)
}

func (n *DynamixelNetwork) Log(format string, v ...interface{}) {
	if n.Debug {
		log.Printf(format, v...)
	}
}
