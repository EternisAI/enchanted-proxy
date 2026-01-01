package problem_reports

import "time"

// CreateProblemReportRequest matches the iOS ProblemReportModel structure.
// Note: id, userId, and createdAt are generated server-side.
// DeviceInfo and StorageInfo are optional - users can opt out of sending device info.
type CreateProblemReportRequest struct {
	ProblemDescription string       `json:"problemDescription" binding:"required"`
	DeviceInfo         *DeviceInfo  `json:"deviceInfo,omitempty"`
	StorageInfo        *StorageInfo `json:"storageInfo,omitempty"`
	SubscriptionTier   *string      `json:"subscriptionTier,omitempty"`
	ContactEmail       *string      `json:"contactEmail,omitempty"` // Optional email so we can reply
}

type DeviceInfo struct {
	DeviceModel   string `json:"deviceModel"`
	DeviceName    string `json:"deviceName"`
	SystemName    string `json:"systemName"`
	SystemVersion string `json:"systemVersion"`
	AppVersion    string `json:"appVersion"`
	BuildNumber   string `json:"buildNumber"`
	Locale        string `json:"locale"`
	Timezone      string `json:"timezone"`
}

type StorageInfo struct {
	TotalCapacityBytes     int64 `json:"totalCapacityBytes"`
	AvailableCapacityBytes int64 `json:"availableCapacityBytes"`
	UsedCapacityBytes      int64 `json:"usedCapacityBytes"`
}

type CreateProblemReportResponse struct {
	ID       string `json:"id"`
	TicketID string `json:"ticketId,omitempty"`
}

type ProblemReport struct {
	ID                 string       `json:"id"`
	UserID             string       `json:"userId"`
	ProblemDescription string       `json:"problemDescription"`
	DeviceInfo         *DeviceInfo  `json:"deviceInfo,omitempty"`
	StorageInfo        *StorageInfo `json:"storageInfo,omitempty"`
	SubscriptionTier   *string      `json:"subscriptionTier,omitempty"`
	ContactEmail       *string      `json:"contactEmail,omitempty"`
	TicketID           *string      `json:"ticketId,omitempty"`
	CreatedAt          time.Time    `json:"createdAt"`
	UpdatedAt          time.Time    `json:"updatedAt"`
}

const (
	MaxReportsPerUser = 100
)
