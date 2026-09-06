package sftp

func (r *Request) getStatFile() StatFile {
	reader, writer, readWriter := r.getAllReaderWriters()
	for _, handle := range [...]any{reader, writer, readWriter, r.getListerAt()} {
		if file, ok := handle.(StatFile); ok {
			return file
		}
	}
	return nil
}

func statFile(id uint32, file StatFile) responsePacket {
	info, err := file.Stat()
	if err != nil {
		return statusFromError(id, err)
	}
	return &sshFxpStatResponse{ID: id, info: info}
}
