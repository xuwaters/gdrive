# GDrive

GDrive is a command line tool that can download files recursively from Google Drive.

```

Usage:
  gdrive download [flags]

Flags:
      --cred_file string    credentials.json file for Google Drive API from gcloud console
                            https://console.developers.google.com/apis/library/drive.googleapis.com
      --dst string          Destination directory
  -h, --help                help for download
      --list_file string    list of files to be downloaded, will be created automatically
      --src string          Source fileId in google drive
      --token_file string   token file that stores access and refresh tokens, and is created automatically

```

## Build

```bash
$ go build -o ./bin/gdrive ./cmd/gdrive
```

## For example
```bash
$ rm -f ./bin/list.json
$ gdrive download --list_file './bin/list.json' --src 'google-drive-folder-or-file-id' --dst 'local-folder-path'
```
