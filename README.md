# GDrive

GDrive is a command line tool that can download files recursively from Google Drive.

```

Usage:
  gdrive download [flags]

Flags:
      --cred-file string    credentials.json file for Google Drive API from gcloud console
                            https://console.developers.google.com/apis/library/drive.googleapis.com
      --dst string          Destination directory
  -h, --help                help for download
      --src string          Source fileId in google drive
      --token-file string   token file that stores access and refresh tokens, and is created automatically

```

## Build

```bash
$ go build -o ./bin/gdrive ./cmd/gdrive
```

## For example
```bash
$ gdrive download --src 'google-drive-folder-or-file-id' --dst 'local-folder-path'
```
