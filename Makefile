TARGET=alertmanager_twilio_gsheets

all: main.go
	CGO_ENABLED=0 go build -o $(TARGET)
clean:
	go clean
	rm -f $(TARGET)