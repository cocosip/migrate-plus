package acceptance

import (
	"embed"
	"fmt"
	"io/fs"
)

//go:embed fixtures/*/*.sql
var fixtureFiles embed.FS

func profiles() []profile {
	definitions := []struct{ driver, mode, family string }{
		{driverOracle, driverOracle, driverOracle},
		{driverDM, modeNative, driverDM}, {driverDM, driverOracle, driverDM}, {driverDM, modeMySQL, driverDM}, {driverDM, modePG, driverDM},
		{driverGaussDB, modeA, modePG}, {driverGaussDB, modePG, modePG},
		{driverOpenGauss, modeA, modePG}, {driverOpenGauss, modePG, modePG},
		{driverOceanBase, modeMySQL, modeMySQL}, {driverOceanBase, driverOracle, driverOracle},
	}
	result := make([]profile, 0, len(definitions))
	for _, d := range definitions {
		id := d.driver + "-" + d.mode
		files, err := fs.Sub(fixtureFiles, "fixtures/"+id)
		if err != nil {
			panic(err)
		} // The embedded paths are checked by profile tests.
		p := profile{ID: id, Driver: d.driver, Mode: d.mode, Family: d.family, FS: files}
		p.Types = nativeTypes(p)
		result = append(result, p)
	}
	return result
}

func profileFor(driver, mode string) (profile, error) {
	for _, p := range profiles() {
		if p.Driver == driver && p.Mode == mode {
			return p, nil
		}
	}
	return profile{}, fmt.Errorf("unsupported migration profile %s/%s", driver, mode)
}
