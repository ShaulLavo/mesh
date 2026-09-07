package updateinstall

func defaultServiceSpec() (ServiceSpec, error) {
	return ServiceSpec{Kind: "systemd", Name: "mesh.service"}, nil
}
