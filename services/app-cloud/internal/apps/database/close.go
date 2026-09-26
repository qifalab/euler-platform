package database

func (m *Module) Close() error {
	if m.cancel != nil {
		m.cancel()
		<-m.done
	}
	var last error
	for _, e := range m.engines {
		if closer, ok := e.Engine.(interface{ Close() error }); ok {
			if err := closer.Close(); err != nil {
				last = err
			}
		}
	}
	return last
}
func (e *sqlEngine) Close() error { return e.db.Close() }
