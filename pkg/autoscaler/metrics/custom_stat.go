package metrics

type CustomStatEntry struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
}

type CustomStat struct {
	CanDownScale bool              `json:"can_down_scale"`
	Stats        []CustomStatEntry `json:"stats"`
	PodName      string
}
