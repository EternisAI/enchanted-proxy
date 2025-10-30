#!/bin/bash

# Test script for creating a task via the proxy server API
# Usage: ./test_create_task.sh
#
# To get the Firebase token:
# 1. Run the iOS app in Xcode simulator
# 2. Sign in to the app
# 3. Check Xcode console for log message starting with '🔑 Firebase ID Token:'
# 4. Copy the entire token string and paste it below

# ===== CONFIGURATION =====
# Paste your Firebase token here (get it from Xcode console logs)
TOKEN="eyJhbGciOiJSUzI1NiIsImtpZCI6IjdlYTA5ZDA1NzI2MmU2M2U2MmZmNzNmMDNlMDRhZDI5ZDg5Zjg5MmEiLCJ0eXAiOiJKV1QifQ.eyJuYW1lIjoiSm9lbCBEcm90bGVmZiIsInBpY3R1cmUiOiJodHRwczovL2xoMy5nb29nbGV1c2VyY29udGVudC5jb20vYS9BQ2c4b2NJa2FfZGpJS0EyN3ZjTXg2ZU1EV0d0Wjgzc1ZaalRhR0gxalhOb1FjbzJCTjlFNU54RD1zOTYtYyIsImlzcyI6Imh0dHBzOi8vc2VjdXJldG9rZW4uZ29vZ2xlLmNvbS9zaWxvLXN0YWdpbmctMmQ3M2YiLCJhdWQiOiJzaWxvLXN0YWdpbmctMmQ3M2YiLCJhdXRoX3RpbWUiOjE3NTk5NjcxODksInVzZXJfaWQiOiJqUHR3aDdudVpYUEJGTFZRVWlUUTNxRDJkRjMzIiwic3ViIjoialB0d2g3bnVaWFBCRkxWUVVpVFEzcUQyZEYzMyIsImlhdCI6MTc2MTg0MTA1OCwiZXhwIjoxNzYxODQ0NjU4LCJlbWFpbCI6ImpvZWxzdGVyOUBnbWFpbC5jb20iLCJlbWFpbF92ZXJpZmllZCI6dHJ1ZSwiZmlyZWJhc2UiOnsiaWRlbnRpdGllcyI6eyJnb29nbGUuY29tIjpbIjEwMDQ2NDI4NDIxMTQxNDgxNjU5MSJdLCJlbWFpbCI6WyJqb2Vsc3RlcjlAZ21haWwuY29tIl19LCJzaWduX2luX3Byb3ZpZGVyIjoiZ29vZ2xlLmNvbSJ9fQ.ZZX_erdVcTE3SPHSzDwuV8JfeWD8_NCdUJc9KtqboMXzUdlb-Be5ixXmKtMBqWjTNs7beQNRmieABrs2-RZHUlD1bRQK7pQAv3I6umnI7glR4PcM7TqHhvWXSvwLauAncXXB25D42e8n1EQMVKcI07aB-C702IQUdyrWzW3jraxqraSok3FBUCZduE4AeoxqiCG34jd9-w5Um-tjri1vtg6ul7YcqPNz_a7SjZkZCKYte57t2-kU0P8m0LNfMp3i4GH3gudPsHO6zPBispUvIupE7m-axkl1eyydsRk0stL7FEM35PcFVRXWEYFglMrxOHFjKMNfwJmROFt1WVtkKA"
BASE_URL="${BASE_URL:-https://proxy-api-staging.ep-use1.ghostagent.org}"

# Check if token has been set
if [ "$TOKEN" = "PASTE_YOUR_FIREBASE_TOKEN_HERE" ]; then
    echo "Error: Please update the TOKEN variable in this script"
    echo "Open the script and paste your Firebase token at line 14"
    echo ""
    echo "To get the token:"
    echo "1. Run the iOS app in Xcode simulator"
    echo "2. Sign in to the app"
    echo "3. Check Xcode console for log message starting with '🔑 Firebase ID Token:'"
    echo "4. Copy the entire token string and paste it into this script"
    exit 1
fi

# Task configuration - modify these as needed
CHAT_ID="${CHAT_ID:-550e8400-e29b-41d4-a716-446655440000}"  # Valid UUID format
TASK_NAME="${TASK_NAME:-Daily Summary}"
TASK_TEXT="${TASK_TEXT:-Send me a summary of today news}"
TASK_TYPE="${TASK_TYPE:-recurring}"
TASK_TIME="${TASK_TIME:-0 9 * * *}"

echo "Creating task..."
echo "Endpoint: $BASE_URL/api/v1/tasks"
echo "Chat ID: $CHAT_ID"
echo "Task Name: $TASK_NAME"
echo "Task Type: $TASK_TYPE"
echo "Task Time: $TASK_TIME"
echo ""

curl -s -X POST "$BASE_URL/api/v1/tasks" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -d "{\"chat_id\":\"$CHAT_ID\",\"task_name\":\"$TASK_NAME\",\"task_text\":\"$TASK_TEXT\",\"type\":\"$TASK_TYPE\",\"time\":\"$TASK_TIME\"}" | jq . || cat

echo ""
