package forgeupdate

type bundlePointer struct {
	root           string
	currentPath    string
	newTarget      string
	previousTarget string
	previousExists bool
}
