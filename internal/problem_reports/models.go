package problem_reports

import "time"

// CreateProblemReportRequest matches the iOS ProblemReportModel structure.
// Note: id, userId, and createdAt are generated server-side.
type CreateProblemReportRequest struct {
	ProblemDescription string      `json:"problemDescription" binding:"required"`
	DeviceInfo         DeviceInfo  `json:"deviceInfo" binding:"required"`
	StorageInfo        StorageInfo `json:"storageInfo" binding:"required"`
	SubscriptionTier   *string     `json:"subscriptionTier,omitempty"`
	ContactEmail       *string     `json:"contactEmail,omitempty"` // Optional email so we can reply
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
	ID         string `json:"id"`
	IsNewIssue bool   `json:"isNewIssue"`
	TicketID   string `json:"ticketId,omitempty"`
	ParentID   string `json:"parentId,omitempty"`
}

type ProblemReport struct {
	ID                 string      `json:"id"`
	UserID             string      `json:"userId"`
	ProblemDescription string      `json:"problemDescription"`
	DeviceInfo         DeviceInfo  `json:"deviceInfo"`
	StorageInfo        StorageInfo `json:"storageInfo"`
	SubscriptionTier   *string     `json:"subscriptionTier,omitempty"`
	ContactEmail       *string     `json:"contactEmail,omitempty"`
	ParentID           *string     `json:"parentId,omitempty"`
	TicketID           *string     `json:"ticketId,omitempty"`
	CreatedAt          time.Time   `json:"createdAt"`
	UpdatedAt          time.Time   `json:"updatedAt"`
}

const (
	MaxReportsPerUser   = 100
	SimilarityThreshold = 0.85
	EmbeddingModel      = "text-embedding-3-small"
	EmbeddingDimensions = 1536
)
