package cli

func serviceAliasRows(rows []ServiceCatalogRow) []ServiceCatalogRow {
	result := make([]ServiceCatalogRow, 0, len(rows))
	for _, row := range rows {
		result = append(result, row)
		if row.Service.PrivateHost == "" {
			continue
		}
		row.Service.PrivateHost = ""
		result = append(result, row)
	}
	return result
}
