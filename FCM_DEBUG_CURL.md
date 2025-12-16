# FCM Debug Curl - How to Use

## What's Logged

When an FCM notification fails, the error log now includes a `debug_curl` field containing the exact curl command that would replicate the request.

## Example Log in Grafana

```json
{
  "error": "unknown error while making an http call: Post \"https://fcm.googleapis.com/v1/projects/silo-dev-95230/messages:send\": Unauthorized",
  "error_type": "*internal.FirebaseError",
  "token_prefix": "ev8H8sVmSE",
  "notification_type": "deep_research",
  "debug_curl": "curl -X POST 'https://fcm.googleapis.com/v1/projects/silo-dev-95230/messages:send' \\\n  -H 'Authorization: Bearer $(gcloud auth application-default print-access-token)' \\\n  -H 'Content-Type: application/json' \\\n  -d '{\"message\":{\"token\":\"ev8H8sVmSE...\",\"notification\":{\"title\":\"Deep Research Complete\",\"body\":\"Your research has finished...\"}}}''"
}
```

## How to Use the Debug Curl

### Option 1: Run with gcloud (Easiest)

If you have `gcloud` CLI installed and authenticated:

```bash
# Copy the curl command from the debug_curl field in Grafana
# Paste and run it directly - it will automatically get the access token

curl -X POST 'https://fcm.googleapis.com/v1/projects/silo-dev-95230/messages:send' \
  -H 'Authorization: Bearer $(gcloud auth application-default print-access-token)' \
  -H 'Content-Type: application/json' \
  -d '{"message":{"token":"ev8H8sVmSE...","notification":{...}}}'
```

### Option 2: Use Python Script with Service Account

If you don't have gcloud but have the service account credentials:

```python
#!/usr/bin/env python3
from google.oauth2 import service_account
from google.auth.transport.requests import Request
import requests
import json

# Your service account credentials
CREDENTIALS_JSON = {...}  # Paste from FIREBASE_CRED_JSON

# Get OAuth token
credentials = service_account.Credentials.from_service_account_info(
    CREDENTIALS_JSON,
    scopes=['https://www.googleapis.com/auth/firebase.messaging']
)
credentials.refresh(Request())
access_token = credentials.token

# The payload from debug_curl
payload = {...}  # Paste from the -d flag in debug_curl

# Send request
response = requests.post(
    'https://fcm.googleapis.com/v1/projects/silo-dev-95230/messages:send',
    headers={
        'Authorization': f'Bearer {access_token}',
        'Content-Type': 'application/json'
    },
    json=payload
)

print(f"Status: {response.status_code}")
print(f"Response: {response.text}")
```

### Option 3: Get Token Manually

1. **Extract the token from the curl command** by running just the token part:
   ```bash
   gcloud auth application-default print-access-token
   ```

2. **Replace** `$(gcloud auth application-default print-access-token)` with the actual token:
   ```bash
   curl -X POST 'https://fcm.googleapis.com/v1/projects/silo-dev-95230/messages:send' \
     -H 'Authorization: Bearer ya29.c.c0AYnqXljS...' \
     -H 'Content-Type: application/json' \
     -d '{"message":{...}}'
   ```

## What the Debug Curl Tells You

### If it succeeds when run manually:
✅ **Credentials are valid**
✅ **IAM permissions are correct**
✅ **FCM API is enabled**
❌ **Problem is with how the Go app loads credentials**

→ Check the startup logs for "FIREBASE CREDENTIALS LOADED" to see what credentials the app is using

### If it fails with 401 Unauthorized:
❌ **Service account lacks FCM permissions**
❌ **Or service account key is revoked**

→ Check IAM roles and service account status

### If it fails with 400 Bad Request:
❌ **Device token is invalid**
❌ **Or message payload is malformed**

→ Check the token and payload format

### If it fails with 404 Not Found:
❌ **FCM API not enabled**
❌ **Or project ID is wrong**

→ Enable FCM API and verify project ID

## Compare with Working Credentials

You can also modify the curl to use the working credentials from the Python test:

```bash
# Get token from working credentials
python3 -c "
from google.oauth2 import service_account
from google.auth.transport.requests import Request

creds = service_account.Credentials.from_service_account_file(
    'path/to/working-creds.json',
    scopes=['https://www.googleapis.com/auth/firebase.messaging']
)
creds.refresh(Request())
print(creds.token)
"

# Use that token in the curl from debug_curl
```

This lets you compare:
- Working credentials → succeeds
- Production credentials → fails

Shows definitively if credentials are the issue!
