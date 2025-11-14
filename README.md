# Caddy Build Server

prototype for a  horizontally scalable build server for creating custom Caddy binaries with specific plugin combinations.

there are two runtime requirements, which are postgres for all three components, and docker for the worker.

go dependencies are somewhat minimal. here is what they are for

1. s3 connectivity (aws sdk+afero)
2. docker connectivity (docker)
3. postgres (pgx,tern, scany)
4. janitor job scheduling (gocron)
5. application lifecycle framework (fx)
7. building/validation (x/mod, x/crypto, semver)


the high level explaination is

1. the api submits job is submitted to postgres. the user is returned a build key to download the job for the future.
2. workers use `update skip locked` to query and lock a build job.
3. the worker builds caddy using a temporary directory. each build command (module resolution, build), are run in a isolated docker environment with said shared temporary directory. this means that the builds are unable to access any files or environement of the build environment.
4. the build binary is uploaded to a "shared storage". in production, this is designeed to be s3/object storage backed, but realistically could be nfs, or if not running distributed/dev, a directory
5. the user should poll the api for this build key (future - server can poll, user can long-poll) until the build is done, and then hit the artifact download
5b. alternatively, use the compat endpoint which hangs the get request until the build is done and returns it.

this is processed by the combination of 3 binaries

api - which hosts the api server. api endpoints can queue jobs, check if job is already queued, check if job is done. the api component is also in charge of resolving latest versions to absolute versions. it has access to the destination store to serve artifacts

worker - gets jobs and does the building in docker, and uploads finished artifacts/signatures to the destination store.

janitor - runs migrations. queues/kills terminally failed jobs for retry. in the future, can do things like, cleanup, gc, etc.

### example request
`curl "http://localhost:8234/api/compat?os=linux&arch=amd64" -o caddy`

## other things

### preemption detection

workers support an still untested "preemption" feature. this is for running on spot vm instances, in where your vm may get preempted and shutdown without warning, in the middle of a job.
the idea is that you can configure workers with a preemption detector, and it will use said preemption detector to know if it is about to be preempted, and if it is, it will stop processing jobs, and mark all its jobs as failed, for another worker to automatically retry.

## Configuration

configuration is loaded from `./config.yml` by default, or from the path specified in the `CONFIG_PATH` environment variable.
it supports env expansion via golang template.

see `config.example.yml` for documentation on the config format

see `config.test.yml` for a local testing config


