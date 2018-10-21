# frameserver

`HTTP GET` frames from `/dev/video0` on your Raspberry Pi

## Quickstart

Cross-compile for Raspberry Pi:

	GOARCH=arm GOOS=linux go build

Copy to Pi, then run:

	./frameserver
