package reels

type SubcategoryFilter struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	Count int    `json:"count"`
}

type CategoryFilter struct {
	Category      string              `json:"category"`
	Label         string              `json:"label"`
	Count         int                 `json:"count"`
	Subcategories []SubcategoryFilter `json:"subcategories"`
}

type TopCategory struct {
	Category *string `json:"category"`
	Label    *string `json:"label"`
	Count    int     `json:"count"`
}

type PlatformFilter struct {
	Platform    string           `json:"platform"`
	Label       string           `json:"label"`
	Count       int              `json:"count"`
	TopCategory *string          `json:"top_category"`
	Categories  []CategoryFilter `json:"categories"`
}

type CategoryFiltersResponse struct {
	UserID               string           `json:"user_id"`
	TotalCount           int              `json:"total_count"`
	TopCategory          TopCategory      `json:"top_category"`
	Categories           []CategoryFilter `json:"categories"`
	SelectedPreviewCount *int             `json:"selected_preview_count"`
	TotalCategories      int              `json:"total_categories"`
}

type PlatformFiltersResponse struct {
	TotalCount           int              `json:"total_count"`
	TopPlatform          *string          `json:"top_platform"`
	SelectedPreviewCount int              `json:"selected_preview_count"`
	Platforms            []PlatformFilter `json:"platforms"`
	Categories           []CategoryFilter `json:"categories"`
}

// BuildCategoryFilters groups facet rows into the category tree. Unlike the
// platform tree, the platform filter here does prune the rows.
func BuildCategoryFilters(userID string, rows []FacetRow, platforms []string, selectedCategory, selectedSubcategory string) CategoryFiltersResponse {
	totalCount := 0
	previewCount := 0
	categoryCounts := map[string]int{}
	subcategoryCounts := map[string]map[string]int{}

	for _, row := range rows {
		if row.Count <= 0 {
			continue
		}
		if len(platforms) > 0 && !contains(platforms, RecordPlatform(row.SourcePlatform)) {
			continue
		}
		category := cleanLabel(optionalString(row.Category), "Other")
		subcategory := cleanLabel(optionalString(row.Subcategory), "Other")

		totalCount += row.Count
		categoryCounts[category] += row.Count
		if subcategoryCounts[category] == nil {
			subcategoryCounts[category] = map[string]int{}
		}
		subcategoryCounts[category][subcategory] += row.Count

		if matchesSelection(category, subcategory, selectedCategory, selectedSubcategory) {
			previewCount += row.Count
		}
	}

	categories := buildCategoryFilters(categoryCounts, subcategoryCounts)
	response := CategoryFiltersResponse{
		UserID:          userID,
		TotalCount:      totalCount,
		Categories:      categories,
		TotalCategories: len(categories),
	}
	if len(categories) > 0 {
		top := categories[0]
		response.TopCategory = TopCategory{Category: &top.Category, Label: &top.Label, Count: top.Count}
	}
	if selectedCategory != "" || selectedSubcategory != "" {
		response.SelectedPreviewCount = &previewCount
	}
	return response
}

// BuildPlatformFilters assembles the platform → category → subcategory tree.
// The selection arguments never prune it; they only drive the preview count.
func BuildPlatformFilters(rows []FacetRow, selectedPlatforms []string, selectedCategory, selectedSubcategory string) PlatformFiltersResponse {
	totalCount := 0
	previewCount := 0
	platformCounts := map[string]int{}
	platformCategoryCounts := map[string]map[string]int{}
	platformSubcategoryCounts := map[string]map[string]map[string]int{}
	categoryCounts := map[string]int{}
	subcategoryCounts := map[string]map[string]int{}

	for _, row := range rows {
		if row.Count <= 0 {
			continue
		}
		platform := RecordPlatform(row.SourcePlatform)
		category := cleanLabel(optionalString(row.Category), "Other")
		subcategory := cleanLabel(optionalString(row.Subcategory), "Other")

		totalCount += row.Count
		platformCounts[platform] += row.Count

		if platformCategoryCounts[platform] == nil {
			platformCategoryCounts[platform] = map[string]int{}
			platformSubcategoryCounts[platform] = map[string]map[string]int{}
		}
		platformCategoryCounts[platform][category] += row.Count
		if platformSubcategoryCounts[platform][category] == nil {
			platformSubcategoryCounts[platform][category] = map[string]int{}
		}
		platformSubcategoryCounts[platform][category][subcategory] += row.Count

		categoryCounts[category] += row.Count
		if subcategoryCounts[category] == nil {
			subcategoryCounts[category] = map[string]int{}
		}
		subcategoryCounts[category][subcategory] += row.Count

		if len(selectedPlatforms) > 0 && !contains(selectedPlatforms, platform) {
			continue
		}
		if !matchesSelection(category, subcategory, selectedCategory, selectedSubcategory) {
			continue
		}
		previewCount += row.Count
	}

	platforms := make([]PlatformFilter, 0, len(platformCounts))
	for _, platform := range sortedByCountThenName(platformCounts, false) {
		categories := buildCategoryFilters(platformCategoryCounts[platform], platformSubcategoryCounts[platform])
		filter := PlatformFilter{
			Platform:   platform,
			Label:      PlatformLabel(platform),
			Count:      platformCounts[platform],
			Categories: categories,
		}
		if len(categories) > 0 {
			filter.TopCategory = &categories[0].Category
		}
		platforms = append(platforms, filter)
	}

	response := PlatformFiltersResponse{
		TotalCount:           totalCount,
		SelectedPreviewCount: previewCount,
		Platforms:            platforms,
		Categories:           buildCategoryFilters(categoryCounts, subcategoryCounts),
	}
	if len(platforms) > 0 {
		response.TopPlatform = &platforms[0].Platform
	}
	return response
}

func buildCategoryFilters(categoryCounts map[string]int, subcategoryCounts map[string]map[string]int) []CategoryFilter {
	categories := make([]CategoryFilter, 0, len(categoryCounts))
	for _, category := range sortedByCountThenName(categoryCounts, true) {
		counts := subcategoryCounts[category]
		subcategories := make([]SubcategoryFilter, 0, len(counts))
		for _, name := range sortedByCountThenName(counts, true) {
			subcategories = append(subcategories, SubcategoryFilter{
				Name:  name,
				Label: Labelize(name),
				Count: counts[name],
			})
		}
		categories = append(categories, CategoryFilter{
			Category:      category,
			Label:         Labelize(category),
			Count:         categoryCounts[category],
			Subcategories: subcategories,
		})
	}
	return categories
}

func matchesSelection(category, subcategory, selectedCategory, selectedSubcategory string) bool {
	if selectedCategory != "" && category != selectedCategory {
		return false
	}
	if selectedSubcategory != "" && subcategory != selectedSubcategory {
		return false
	}
	return true
}
