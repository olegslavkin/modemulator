//////////////////////////////////////////////////////////////////////////

package main

//////////////////////////////////////////////////////////////////////////

import (
	"bytes"
	"errors"
	//	"fmt"
	log "github.com/sirupsen/logrus"
	"io"
	"net"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"time"
)

//////////////////////////////////////////////////////////////////////////
// some config options

const PPS_TARGET = 10

const SERVER_BIND = ""
const SERVER_PORT_INDEX = 10000

const TELNET_SERVER = "localhost"

var BAUDS []uint = []uint{
	300,
	1200,
	2400,
	4800,
	9600,
	14400,
	19200,
	28800,
	33600,
	56000,
	115000,
}

var AddressBook map[string]string = map[string]string{
	"54311": "shell.fr-par1.burble.dn42:23",
	"4242":  "localhost:23",
}

var OnlyNumbers *regexp.Regexp = regexp.MustCompile("([^0-9]+)")

// global data and structures

var Listeners []net.Listener

type TCode uint

const (
	TN_NORM TCode = 65535
	TN_IAC  TCode = 255
	TN_DONT TCode = 254
	TN_DO   TCode = 253
	TN_WONT TCode = 252
	TN_WILL TCode = 251
	TN_SB   TCode = 250
	TN_GA   TCode = 249
	TN_EL   TCode = 248
	TN_EC   TCode = 247
	TN_AYT  TCode = 246
	TN_AO   TCode = 245
	TN_IP   TCode = 244
	TN_BRK  TCode = 243
	TN_DM   TCode = 242
	TN_NOP  TCode = 241
	TN_SE   TCode = 240
)

type DTEMode uint

const (
	DTE_COMMAND DTEMode = iota
	DTE_COMMAND_A
	DTE_COMMAND_AT
	DTE_ATTN_1
	DTE_ATTN_2
	DTE_ATTN_3
	DTE_IAC
	DTE_CONNECTED
)

type Modem struct {
	baud      uint
	dce       net.Conn
	mode      DTEMode
	echo      bool
	cmdBuff   *bytes.Buffer
	dteBuff   []byte
	connected bool
	dte       net.Conn
	state     TCode
	readSlot  time.Time
	writeSlot time.Time
	dceBuff   []byte
}

var Modems map[string]*Modem

//////////////////////////////////////////////////////////////////////////
// utility function to set the log level

func setLogLevel(levelStr string) {

	if level, err := log.ParseLevel(levelStr); err != nil {
		// failed to set the level

		// set a sensible default and, of course, log the error
		log.SetLevel(log.InfoLevel)
		log.WithFields(log.Fields{
			"loglevel": levelStr,
			"error":    err,
		}).Error("Failed to set requested log level")

	} else {

		// set the requested level
		log.SetLevel(level)

	}
}

//////////////////////////////////////////////////////////////////////////
// listen on required ports

func Listen() {

	for _, baud := range BAUDS {
		port := SERVER_PORT_INDEX + (baud / 100)
		bind := SERVER_BIND + ":" + strconv.Itoa(int(port))

		log.WithFields(log.Fields{
			"bind": bind,
			"baud": baud,
		}).Debug("Listening on socket")

		listener, err := net.Listen("tcp", bind)
		if err != nil {
			log.WithFields(log.Fields{
				"bind":  bind,
				"baud":  baud,
				"error": err,
			}).Fatal("Failed to bind listening port")
		}

		Listeners = append(Listeners, listener)

		// spin off a thread waiting for new connections
		go func(p uint, b uint) {
			for {

				conn, err := listener.Accept()
				if err != nil {
					if errors.Is(err, net.ErrClosed) {
						log.WithFields(log.Fields{
							"port": p,
						}).Debug("Listener closed")
						return
					} else {
						log.WithFields(log.Fields{
							"port":  p,
							"error": err,
						}).Error("Listen Error")
						return
					}
				}

				// initialise modem and start listening for commands
				modem := &Modem{}
				go modem.DTEReceive(b, conn)
			}
		}(port, baud)
	}

}

//////////////////////////////////////////////////////////////////////////
// modem utility functions

func (m *Modem) disconnect() {
	if m.connected {
		m.dce.Close()
	}
	m.connected = false
	m.mode = DTE_COMMAND
}

func (m *Modem) Shutdown() {
	m.disconnect()
	m.dte.Close()
}

func (m *Modem) bytesPerTimeslot() uint {
	return (m.baud / (PPS_TARGET * 9)) + 1
}

func (m *Modem) createBuffers() {
	m.dteBuff = make([]byte, m.baud)
	m.dceBuff = make([]byte, m.bytesPerTimeslot())
	if m.cmdBuff == nil {
		m.cmdBuff = new(bytes.Buffer)
		m.cmdBuff.Grow(80)
	}
}

func (m *Modem) dteWrite(data []byte) {
	if _, err := m.dte.Write(data); err != nil {
		m.Shutdown()
	}
}

func (m *Modem) dceWrite(data []byte) {

	// check I'm actually connected
	if !m.connected {
		return
	}

	// step through the data in chunks
	max := int(m.bytesPerTimeslot())

	for scan := 0; scan < len(data); {

		// figure out what to send
		end := scan + max
		if end > len(data) {
			end = len(data)
		}
		chunk := data[scan:end]

		// wait if necessary until the write timeslot
		now := time.Now()
		if m.writeSlot.After(now) {
			wtime := m.writeSlot.Sub(now)
			time.Sleep(wtime)
		}

		// send the data
		if _, err := m.dce.Write(chunk); err != nil {
			m.disconnect()
			m.noCarrier()
			return
		}

		// set the next available timeslot
		tlen := time.Duration((9 * 1000 * 1000 * 1000 * len(chunk)) / int(m.baud))
		m.writeSlot = now.Add(tlen)

		// and move on
		scan += len(chunk)
	}
}

func (m *Modem) tnWrite(codes []TCode) {
	b := make([]byte, len(codes))
	for i, c := range codes {
		b[i] = byte(c)
	}
	m.dceWrite(b)
}

func (m *Modem) cmdOK() {
	m.dteWrite([]byte("OK\r\n"))
}

func (m *Modem) cmdError() {
	m.dteWrite([]byte("ERROR\r\n"))
}

func (m *Modem) noCarrier() {
	m.dteWrite([]byte("NO CARRIER\r\n"))
}

func (m *Modem) connect(dial string) error {

	endpoint, found := AddressBook[dial]
	if !found {
		return errors.New("Address not found:" + dial)
	}

	conn, err := net.Dial("tcp6", endpoint)
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("Telnet connection failed")
		m.connected = false
		return err
	}
	m.dce = conn
	m.connected = true
	m.mode = DTE_CONNECTED
	return nil
}

//////////////////////////////////////////////////////////////////////////
// Modem receive thread

func (m *Modem) DTEReceive(baud uint, dte net.Conn) {

	remote := dte.RemoteAddr().String()
	log.WithFields(log.Fields{
		"remote": remote,
	}).Info("Accepting new modem")

	// initialise state
	m.baud = baud
	m.dte = dte
	m.createBuffers()
	m.atCmd("z")

	// register
	Modems[remote] = m
	defer func() {
		log.WithFields(log.Fields{
			"remote": remote,
		}).Debug("Shutting down modem")
		m.Shutdown()
		delete(Modems, remote)
	}()

	for {

		// read some data

		if m.mode == DTE_ATTN_3 {
			// potential hangup, set a read deadline
			m.dte.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		}

		// do the read
		available, err := m.dte.Read(m.dteBuff)

		if m.mode == DTE_ATTN_3 {
			// clear the deadline and return to connected state
			m.dte.SetReadDeadline(time.Time{})

			if (err != nil) && errors.Is(err, os.ErrDeadlineExceeded) {
				// back to command mode
				m.mode = DTE_COMMAND
				m.cmdOK()
				err = nil

			} else {
				// no attention
				m.mode = DTE_CONNECTED
				m.dce.Write([]byte("+++"))
			}
		}

		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				log.WithFields(log.Fields{
					"remote": remote,
				}).Info("Modem DTE closed")
				return
			} else {
				log.WithFields(log.Fields{
					"remote": remote,
					"error":  err,
				}).Error("DTE Read error")
				return
			}
		}

		for scan := 0; scan < available; {
			switch m.mode {

			case DTE_COMMAND:
				// waiting for AT

				start := scan
				for ; scan < available; scan++ {
					if m.dteBuff[scan] == 'a' || m.dteBuff[scan] == 'A' {
						m.mode = DTE_COMMAND_A
						scan++
						break
					}
				}

				// echo characters if required
				if m.echo {
					m.dteWrite(m.dteBuff[start:scan])
				}

			case DTE_COMMAND_A:
				// have A, is next byte a T ?
				if m.dteBuff[scan] == 't' || m.dteBuff[scan] == 'T' {
					// yes, start AT command mode
					m.mode = DTE_COMMAND_AT
					scan++

					// echo char if needed
					if m.echo {
						m.dteWrite(m.dteBuff[scan-1 : scan])
					}

				} else {
					// not a T, reset the mode
					m.mode = DTE_COMMAND
				}

			case DTE_COMMAND_AT:
				// have AT, waiting for \r

				eol := false
				start := scan
				for ; scan < available; scan++ {
					if m.dteBuff[scan] == '\r' {
						// signal EOL
						eol = true
						break
					}
				}

				data := m.dteBuff[start:scan]

				// echo characters if required
				if m.echo {
					m.dteWrite(data)
				}

				// add to command buffer
				if (m.cmdBuff.Len() + len(data)) > 80 {
					// nah too long
					m.cmdBuff.Reset()
					m.cmdError()
				} else {
					m.cmdBuff.Write(data)
				}

				// parse AT command if EOL
				if eol {
					scan++
					m.dteWrite([]byte("\r\n"))

					cmd := m.cmdBuff.String()
					log.WithFields(log.Fields{
						"remote": remote,
						"cmd":    cmd,
					}).Debug("ATCMD")

					m.atCmd(cmd)
					m.cmdBuff.Reset()
					if m.mode != DTE_CONNECTED {
						m.mode = DTE_COMMAND
					}
				}

			case DTE_CONNECTED:
				// stream to DCE

				// look ahead for control codes
				start := scan
			SCAN:
				for ; scan < available; scan++ {
					switch m.dteBuff[scan] {
					case '+':
						m.mode = DTE_ATTN_1
						break SCAN
					case byte(TN_IAC):
						m.mode = DTE_IAC
						break SCAN
					}
				}

				// write data so far
				data := m.dteBuff[start:scan]
				m.dceWrite(data)

				if scan < available {
					// + or TN_IAC was received, skip over it
					scan++
				}

			case DTE_IAC:
				// escape TN_IAC, _don't_ move scan as it
				// has already been done in DTE_CONNECTED
				m.dceWrite([]byte{byte(TN_IAC), byte(TN_IAC)})
				m.mode = DTE_CONNECTED

			case DTE_ATTN_1:
				if m.dteBuff[scan] == '+' {
					m.mode = DTE_ATTN_2
					scan++
				} else {
					// no attention, send suitable number of +
					m.mode = DTE_CONNECTED
					m.dceWrite([]byte("+"))
				}

			case DTE_ATTN_2:
				if m.dteBuff[scan] == '+' {
					scan++
					if scan < available {
						// more bytes were available, no hangup
						m.mode = DTE_CONNECTED
						m.dceWrite([]byte("+++"))
					} else {
						// wait for pause
						m.mode = DTE_ATTN_3
					}

				} else {
					// no attention, send suitable number of +
					m.mode = DTE_CONNECTED
					m.dceWrite([]byte("++"))
				}
			}
		}
	}
}

//////////////////////////////////////////////////////////////////////////
// action AT command

func (m *Modem) atCmd(cmd string) {
	cmd = strings.ToLower(strings.TrimSpace(cmd))
	l := len(cmd)

	// cope with zero length commands
	if l == 0 {
		m.cmdError()
		return
	}

	switch cmd[0] {
	case 'd':
		// dial

		if m.connected || l < 4 {
			m.cmdError()
			return
		}

		// remove any non-digit characters
		dial := OnlyNumbers.ReplaceAllString(cmd, "")
		log.WithFields(log.Fields{
			"dial": dial,
		}).Debug("Dial number")

		if err := m.connect(dial); err != nil {
			m.noCarrier()
			return
		}

		// show connect string
		cstr := "CONNECT " + strconv.Itoa(int(m.baud)) + "\r\n"
		m.dte.Write([]byte(cstr))

		// start dce read thread
		go m.dceTelnet()

	case 'h':
		// hangup
		switch {
		case l == 1 || cmd[1] == '0':
			m.disconnect()
			m.cmdOK()
		default:
			m.cmdError()
		}

	case 'e':
		// enable/disable echo
		switch {
		case l == 1 || cmd[1] == '0':
			m.echo = false
			m.cmdOK()
		case cmd[1] == '1':
			m.echo = true
			m.cmdOK()
		default:
			m.cmdError()
		}

	case 'o':
		if l == 1 {
			if m.connected {
				m.mode = DTE_CONNECTED
				m.cmdOK()
			} else {
				m.noCarrier()
			}
		} else {
			m.cmdError()
		}

	case 'z':
		if l == 1 || cmd[1] == '0' {
			m.disconnect()
			m.echo = true
			m.mode = DTE_COMMAND
			m.cmdBuff.Reset()
			m.cmdOK()
		} else {
			m.cmdError()
		}

	default:
		m.cmdOK()
	}

}

//////////////////////////////////////////////////////////////////////////
// DCE read thread

func (m *Modem) dceTelnet() {

	defer func() {
		log.WithFields(log.Fields{
			"remote": m.dte.RemoteAddr().String(),
		}).Debug("Shutting down DCE")
		m.disconnect()
		m.noCarrier()
	}()

	// suppress GA (3)
	// binary transmission (0)
	// echo (1)
	m.tnWrite([]TCode{
		TN_IAC, TN_WILL, TCode(3),
		TN_IAC, TN_DO, TCode(3),
		TN_IAC, TN_WILL, TCode(0),
		TN_IAC, TN_DO, TCode(0),
		TN_IAC, TN_WONT, TCode(1),
		TN_IAC, TN_DO, TCode(1),
	})

	m.state = TN_NORM

	for {

		// wait if necessary until we can read
		now := time.Now()
		if m.readSlot.After(now) {
			rtime := m.readSlot.Sub(now)
			time.Sleep(rtime)
		}

		// read some data
		available, err := m.dce.Read(m.dceBuff)

		// set the next available timeslot
		tlen := time.Duration((9 * 1000 * 1000 * 1000 * len(m.dceBuff)) / int(m.baud))
		m.readSlot = now.Add(tlen)

		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				log.WithFields(log.Fields{
					"remote": m.dte.RemoteAddr().String(),
				}).Info("Telnet DCE closed")
				return
			} else {
				log.WithFields(log.Fields{
					"remote": m.dte.RemoteAddr().String(),
					"error":  err,
				}).Error("DCE Read error")
				return
			}
		}

		// parse the read data for control chars
		for scan := 0; scan < available; {
			switch m.state {
			case TN_NORM:
				// normal data processing, scan for IAC code

				start := scan
				for ; scan < available; scan++ {
					if TCode(m.dceBuff[scan]) == TN_IAC {
						m.state = TN_IAC
						break
					}
				}

				// write data so far
				data := m.dceBuff[start:scan]
				m.dte.Write(data)

				if scan < available {
					// TN_IAC was received, skip over it
					scan++
				}

			case TN_IAC:
				// have received IAC look for the next code
				code := TCode(m.dceBuff[scan])
				scan++

				switch code {
				case TN_IAC:
					// actually send 255 and reset
					m.dte.Write([]byte{byte(TN_IAC)})
					m.state = TN_NORM
				case TN_WILL:
					m.state = TN_WILL
				case TN_WONT:
					m.state = TN_WONT
				case TN_DO:
					m.state = TN_DO
				case TN_DONT:
					m.state = TN_DO
				default:
					log.WithFields(log.Fields{
						"code": code,
					}).Debug("Un-implemented TN code")
					m.state = TN_NORM
				}

			case TN_WILL:

				code := TCode(m.dceBuff[scan])
				m.state = TN_NORM
				scan++

				switch code {
				case 0:
					// ignore binary confirmation
				case 1:
					// ignore echo
				case 3:
					// ignore GA confirmation
				default:
					// refuse any other WILL codes
					m.tnWrite([]TCode{TN_IAC, TN_DONT, code})
					log.WithFields(log.Fields{
						"code": code,
					}).Debug("Un-implemented TN_WILL")
				}

			case TN_WONT:

				code := TCode(m.dceBuff[scan])
				m.state = TN_NORM
				scan++

				switch code {
				default:
					log.WithFields(log.Fields{
						"code": code,
					}).Debug("Un-implemented TN_WONT")
				}

			case TN_DO:

				code := TCode(m.dceBuff[scan])
				m.state = TN_NORM
				scan++

				switch code {
				case 0:
					// ignore binary confirmation
				case 1:
					// won't echo
					m.tnWrite([]TCode{TN_IAC, TN_WONT, code, TN_IAC, TN_DO, code})
				case 3:
					// ignore GA confirmation
				default:
					// refuse any DO codes
					m.tnWrite([]TCode{TN_IAC, TN_WONT, code})
					log.WithFields(log.Fields{
						"code": code,
					}).Debug("Un-implemented TN_DO")
				}

			case TN_DONT:

				code := TCode(m.dceBuff[scan])
				m.state = TN_NORM
				scan++

				switch code {
				default:
					// confirm won't do any codes
					m.tnWrite([]TCode{TN_IAC, TN_WONT, code})
					log.WithFields(log.Fields{
						"code": code,
					}).Debug("Un-implemented TN_DONT")
				}

			default:
				log.WithFields(log.Fields{
					"state": m.state,
				}).Debug("Unknown TN state")
			}

		}
	}

}

//////////////////////////////////////////////////////////////////////////
// everything starts here

func main() {

	log.SetLevel(log.DebugLevel)
	log.Info("modemulator starting")

	// startup
	Modems = make(map[string]*Modem)
	Listen()

	// graceful shutdown via SIGINT (^C)
	csig := make(chan os.Signal, 1)
	signal.Notify(csig, os.Interrupt)

	// and block
	<-csig

	log.Info("Server shutting down")

	// shutdown listeners here
	for _, listener := range Listeners {
		listener.Close()
	}

	// shutdown active modems
	for _, modem := range Modems {
		modem.Shutdown()
	}

	// nothing left to do
	log.Info("Shutdown complete, all done")
	os.Exit(0)
}

//////////////////////////////////////////////////////////////////////////
// end of code
