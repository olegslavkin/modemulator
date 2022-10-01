# modemulator

[![Build Status](https://ci.burble.dn42/api/badges/burble.dn42/modemulator/status.svg?ref=refs/heads/main)](https://ci.burble.dn42/burble.dn42/modemulator)

modemulator is small golang server that emulates a small subset of the
Hayes AT command set. The server can be configured to connect to other backend
services via the telnet protocol when a user dials specific numbers. 

The server is intended to be used with a serial terminal or via a virtual serial port
in a virtual machine to emulate a modem connection to a remote server.

| Port | BAUD | Port | BAUD | Port | BAUD |
|:--|:--|:--|:--|:--|:--|
| _10003_ | 300   | _10012_ | 1200   | _10024_ | 2400 |
| _10048_ | 4800  | _10096_ | 9600   | _10144_ | 14400 |
| _10192_ | 19200 | _10288_ | 28800  | _10336_ | 33600 |
| _10560_ | 56000 | _11150_ | 115000 |

## Connecting using socat and minicom

Example on how to connect to the shell server using socat and minicom at 2400 baud

```text
$ socat -d -d pty,rawer tcp:dialup.burble.dn42:10024 &
...
... socat[xxx] N PTY is /dev/pts/2
...
$ minicom -D /dev/pts/2

```

Within minicom type **ATDT54311** to dial the shell server.

```text
Welcome to minicom 2.8

OPTIONS: I18n 
Port /dev/pts/2, 14:01:58

Press CTRL-A Z for help on special keys

ATDT54311
CONNECT 2400
Ubuntu 22.04.1 LTS
shell-fr-par1 login: 
```

## Connecting using qemu

Add a virtual serial port to QEMU using `-chardev` and `-device`.

e.g. 

```text
-chardev socket,id=bdn42,port=11150,host=dialup.burble.dn42,server=off -device pci-serial,chardev=bdn42
```

