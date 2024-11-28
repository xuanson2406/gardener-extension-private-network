/*
Package flavors provides information and interaction with Flavors
for the OpenStack Load-balancing service.

Example to List Flavors

	listOpts := flavors.ListOpts{}

	allPages, err := flavors.List(octaviaClient, listOpts).AllPages()
	if err != nil {
		panic(err)
	}

	allFlavors, err := flavors.ExtractFlavors(allPages)
	if err != nil {
		panic(err)
	}

	for _, flavor := range allFlavors {
		fmt.Printf("%+v\n", flavor)
	}

Example to Create a Flavor

	createOpts := flavors.CreateOpts{
		Name:            "Flavor name",
		Description:     "My flavor description",
		Enable:          true,
		FlavorProfileId: "9daa2768-74e7-4d13-bf5d-1b8e0dc239e1",
	}

	flavor, err := flavors.Create(octaviaClient, createOpts).Extract()
	if err != nil {
		panic(err)
	}

Example to Update a Flavor

	flavorID := "d67d56a6-4a86-4688-a282-f46444705c64"

	updateOpts := flavors.UpdateOpts{
		Name: "New name",
	}

	flavor, err := flavors.Update(octaviaClient, flavorID, updateOpts).Extract()
	if err != nil {
		panic(err)
	}

Example to Delete a Flavor

	flavorID := "d67d56a6-4a86-4688-a282-f46444705c64"
	err := flavors.Delete(octaviaClient, flavorID).ExtractErr()
	if err != nil {
		panic(err)
	}
*/
package flavors
