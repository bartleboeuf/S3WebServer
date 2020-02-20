# S3WebServer

This application is a simple HTTP WebServer based on a S3-backed persistent storage.

This allow you to have a virtual host with a root directory in a S3 bucket.

Currently it support HTTP verbs POST/GET/PUT/DELETE.

## Configuration 

You need to configure in a configuration file (in Yaml or Json or Toml) the following properties :

- `port` : The port number the application will listen on.

*Optional - Default: 8000*

- `awsRegion` : The AWS region the bucket resides in.

*Optional - Default: eu-west-1*

- `s3bucket` : The name of the bucket.

*Mandatory - Application will exit if not present*

- `homepage` : The directory index page of a directory if not specified

*Optional - Application will return a http error 400 *

## Running
The application requires several environment variables in order to run.
Below is an example execution:

```
AWS_ACCESS_KEY_ID=<yourAccessKeyId> \
AWS_SECRET_ACCESS_KEY=<yourSecretAccessKey> \
./s3webserver -config config.toml
```
