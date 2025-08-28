#!/usr/bin/env python3
"""
Sync OpenRouter analytics to PostHog.
Fetches daily user activity data and sends it as a structured event.

Usage:
  # Sync yesterday (default)
  python sync_openrouter_analytics.py
  
  # Sync a specific date
  python sync_openrouter_analytics.py --date 2025-08-25
  
  # Sync a date range (backfill)
  python sync_openrouter_analytics.py --start-date 2025-08-20 --end-date 2025-08-25
  
  # Sync from start date to today
  python sync_openrouter_analytics.py --start-date 2025-08-20

Required environment variables:
  OPENROUTER_API_KEY - OpenRouter provisioning API key
  POSTHOG_API_KEY - PostHog API key  
  POSTHOG_HOST - PostHog host URL (e.g., https://us.i.posthog.com)
"""

import os
import sys
import argparse
from datetime import datetime, timedelta, timezone
from typing import Dict, Any, List
import requests


def get_yesterday() -> str:
    """Get yesterday's date in YYYY-MM-DD format."""
    yesterday = datetime.now(timezone.utc) - timedelta(days=1)
    return yesterday.strftime('%Y-%m-%d')


def fetch_openrouter_activity(api_key: str, date: str) -> List[Dict[str, Any]]:
    """Fetch OpenRouter activity data for a specific date."""
    url = "https://openrouter.ai/api/v1/activity"
    headers = {"Authorization": f"Bearer {api_key}"}
    params = {"date": date}
    
    response = requests.get(url, headers=headers, params=params)
    response.raise_for_status()
    
    data = response.json()
    if "error" in data:
        raise Exception(f"OpenRouter API error: {data['error']}")
    
    return data.get("data", [])


def transform_activity_data(raw_data: List[Dict[str, Any]], date: str) -> Dict[str, Any]:
    """Transform OpenRouter user activity data into structured PostHog event."""
    if not raw_data:
        return {
            "date": date,
            "total_cost": 0,
            "total_requests": 0,
            "total_prompt_tokens": 0,
            "total_completion_tokens": 0,
            "total_reasoning_tokens": 0,
            "model_count": 0,
            "models": []
        }
    
    total_cost = sum(item["usage"] for item in raw_data)
    total_requests = sum(item["requests"] for item in raw_data)
    total_prompt_tokens = sum(item["prompt_tokens"] for item in raw_data)
    total_completion_tokens = sum(item["completion_tokens"] for item in raw_data)
    total_reasoning_tokens = sum(item.get("reasoning_tokens", 0) for item in raw_data)
    
    models = []
    for item in raw_data:
        model = {
            "name": item.get("model_permaslug", item.get("model", "")),
            "cost": item["usage"],
            "requests": item["requests"],
            "prompt_tokens": item["prompt_tokens"],
            "completion_tokens": item["completion_tokens"],
            "reasoning_tokens": item.get("reasoning_tokens", 0),
            "byok_usage": item.get("byok_usage_inference", 0),
            "provider": item["provider_name"]
        }
        models.append(model)
    
    # Sort models by cost descending
    models.sort(key=lambda x: x["cost"], reverse=True)
    
    return {
        "date": date,
        "total_cost": total_cost,
        "total_requests": total_requests,
        "total_prompt_tokens": total_prompt_tokens,
        "total_completion_tokens": total_completion_tokens,
        "total_reasoning_tokens": total_reasoning_tokens,
        "model_count": len(models),
        "models": models
    }


def send_to_posthog(api_key: str, host: str, event_data: Dict[str, Any], date: str) -> None:
    """Send event to PostHog."""
    url = f"{host}/i/v0/e/"
    
    timestamp = f"{date}T23:30:00Z"
    
    payload = {
        "api_key": api_key,
        "event": "openrouter_daily_activity",
        "distinct_id": "enchanted_github_actions",
        "properties": event_data,
        "timestamp": timestamp
    }
    
    response = requests.post(url, json=payload)
    response.raise_for_status()
    
    result = response.json()
    if result.get("status") != "Ok":
        raise Exception(f"PostHog API error: {result}")


def parse_date_range(start_date: str, end_date: str = None) -> List[str]:
    """Parse date range and return list of dates."""
    dates = []
    start = datetime.strptime(start_date, '%Y-%m-%d')
    
    if end_date:
        end = datetime.strptime(end_date, '%Y-%m-%d')
        current = start
        while current <= end:
            dates.append(current.strftime('%Y-%m-%d'))
            current += timedelta(days=1)
    else:
        dates.append(start.strftime('%Y-%m-%d'))
    
    return dates


def sync_date(openrouter_key: str, posthog_key: str, posthog_host: str, date: str) -> None:
    """Sync data for a single date."""
    print(f"Fetching OpenRouter activity for {date}")
    
    try:
        raw_data = fetch_openrouter_activity(openrouter_key, date)
        print(f"Found {len(raw_data)} activity records")
        
        event_data = transform_activity_data(raw_data, date)
        print(f"Processed data: ${event_data['total_cost']:.2f} total cost, {event_data['model_count']} models")
        
        send_to_posthog(posthog_key, posthog_host, event_data, date)
        print("✅ Successfully sent data to PostHog")
        
    except requests.RequestException as e:
        print(f"❌ Network error for {date}: {e}")
        raise
    except Exception as e:
        print(f"❌ Error for {date}: {e}")
        raise


def main():
    """Main function."""
    parser = argparse.ArgumentParser(description="Sync OpenRouter analytics to PostHog")
    parser.add_argument("--date", help="Specific date to sync (YYYY-MM-DD). Defaults to yesterday.")
    parser.add_argument("--start-date", help="Start date for range sync (YYYY-MM-DD)")
    parser.add_argument("--end-date", help="End date for range sync (YYYY-MM-DD)")
    
    args = parser.parse_args()
    
    openrouter_key = os.getenv("OPENROUTER_API_KEY")
    posthog_key = os.getenv("POSTHOG_API_KEY")
    posthog_host = os.getenv("POSTHOG_HOST")
    
    if not all([openrouter_key, posthog_key, posthog_host]):
        print("Error: Missing required environment variables")
        print("Required: OPENROUTER_API_KEY, POSTHOG_API_KEY, POSTHOG_HOST")
        sys.exit(1)
    
    if args.start_date:
        dates = parse_date_range(args.start_date, args.end_date)
        print(f"Syncing date range: {dates[0]} to {dates[-1]} ({len(dates)} days)")
    elif args.date:
        dates = [args.date]
        print(f"Syncing specific date: {args.date}")
    else:
        dates = [get_yesterday()]
        print(f"Syncing yesterday (default): {dates[0]}")
    
    success_count = 0
    for date in dates:
        try:
            sync_date(openrouter_key, posthog_key, posthog_host, date)
            success_count += 1
        except Exception:
            print(f"Skipping {date} due to error")
            continue
    
    print(f"\n🎯 Summary: {success_count}/{len(dates)} dates synced successfully")


if __name__ == "__main__":
    main()