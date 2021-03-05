# alertmanager-twilio-gsheets

This [alertmanager](https://github.com/prometheus/alertmanager) webhook is designed to send SMS alerts using [twilio](https://www.twilio.com/) to phone numbers read from Google Sheets.

```
Alertmanager
+-------------------------------+
|           /!\                 |
|                               |
| team:    "infrastructure"     |
| summary: "Server is burning"  |
|                               |
+------------+------------------+  Google Sheets
             |                     +----------------+---------------------------+
             |                     |      TEAM      |        PHONE NUMBERS      |
+------------v----------------+    +--------------------------------------------+
|                             |    | red            | 33611111111 |             |
| alertmanager-twilio-gsheets +----+ infrastructure | 33333333333 | 33666666666 |
|                             |    | blue           | 33888888888 |             |
+------------+----------------+    | green          |             |             |
             |                     +----------------+-------------+-------------+
             |
             |
             |      Twilio         +333 33 33 33 33 : "firing: Server is burning"
             +------------------>
                                   +336 66 66 66 66 : "firing: Server is burning"
```

## Compile

[GOPATH related doc](https://golang.org/doc/code.html#GOPATH)
```bash
export GOPATH="your go path"
make
```

## Usage

1. Create a [twilio API key](https://www.twilio.com/console/project/api-keys) and save the token's ```SID and token``` as well as your ```account SID``` and your sender's ```phone number```

2. Get a Google service account API token:
    1. Create a [Google developer project](https://console.developers.google.com/apis/dashboard)
    2. Enable [Google Sheets API](https://console.developers.google.com/apis/library/sheets.googleapis.com) for the project
    3. Create a Service Account
    4. Add a new key for the Service Account, you should get a json token file
    5. Note the service account's email address (xxxx@yyyy.iam.gserviceaccount.com)

3. Create a Google Sheet with the same format as [this one](https://docs.google.com/spreadsheets/d/18NWlDKn8WJFjHAdm8KKbWHs4xkubnbivYsowSl1Je8M/edit?usp=sharing) (or simply make a copy)
    1. Share it with your service account's email address noted earlier, with viewer access
    2. Note the Sheet's ID present in its URL (https://docs.google.com/spreadsheets/d/XXXXXXXXXXX/)

2. Populated the needed environment variables:
    ```bash
    cp .env.default .env
    chmod o-r .env
    ... # Edit the file with the values that you got earlier
    source .env
    ```

3. Run ```alertmanager_twilio_gsheets```.

### Parameters

* `TWILIO_ACCOUNT_SID` - (required) your twilio account SID
* `TWILIO_AUTH_SID` - (required) your API token's SID
* `TWILIO_AUTH_TOKEN` - (required) your API token
* `TWILIO_FROM_NUMBER` - (required) the phone number registered to send SMS e.g. "+33611223344"
* `GOOGLE_SHEET_ID` - (required) your Google sheet's ID found in its URL
* `GOOGLE_TOKEN_PATH` - (required) the path to your Google service account token
* `PORT` - (optional) the listening port (default 9080)
* `SENTRY_DSN` - (optional) a Sentry project DSN for errors logging

### Configuring alertmanager

Alert manager configuration file:

```yaml
receivers:
- name: 'twilio'
  webhook_configs:
  - url: 'http://127.0.0.1:9080/webhook'
```

## Sending SMS alerts

One message per firing alert and resolve notice is sent to all matching phone numbers.

### Labels and annotations

The ```summary``` annotation is used as the alert's message.

A ```team``` label is expected to match with a row on the spreadsheet.

### Cache

To avoid Google API rate-limit, cache is used to store phone numbers and expires every 10 minutes.  
In the same way, another cache layer is used as fallback when Google Sheet cannot be read.

## Sentry

This project uses [Sentry](https://sentry.io/welcome/) to log error messages and crash stacktraces.  
If you also use it, simply use the `SENTRY_DSN` parameter!
