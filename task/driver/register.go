package driver

import (
	"sync"
)

var (
	driversMu sync.RWMutex
	drivers   = make(map[string]TaskDriver)
)

type TaskPair struct {
	Name   string
	Driver TaskDriver
}

func Register(name string, driver TaskDriver) {
	driversMu.Lock()
	defer driversMu.Unlock()
	if driver == nil {
		panic("greatws.task: Register driver is nil")
	}
	if _, dup := drivers[name]; dup {
		panic("greatws.task: Register called twice for driver " + name)
	}
	drivers[name] = driver
}

func GetAllRegister() []TaskPair {
	rv := make([]TaskPair, 0, len(drivers))

	driversMu.RLock()
	defer driversMu.RUnlock()
	for name, driver := range drivers {
		rv = append(rv, TaskPair{
			Name:   name,
			Driver: driver,
		})
	}
	return rv
}
func GetRegister(name string) TaskDriver {

	driversMu.RLock()
	driver, ok := drivers[name]
	if !ok {
		// 没有找到
		panic("greatws.task: unknown driver " + name)
	}
	defer driversMu.RUnlock()

	return driver
}
