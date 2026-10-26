# Third-Party Notices

migrate-plus is licensed under its own [MIT license](LICENSE), copyright
2026 cocosip. The notices below describe the derived CLI files and native client dependencies.

## golang-migrate CLI

`cli/main.go`, `cli/commands.go`, and `cli/log.go` are derived from
`internal/cli/{main,commands,log}.go` in
[golang-migrate/migrate v4.20.1](https://github.com/golang-migrate/migrate/tree/v4.20.1/internal/cli),
revision `504568a3cbd23b8754760f55a3d89aec1b0c4963`.

Go's internal-package rules prevent importing that command layer from another
module. The migration core, interfaces, registries, and source drivers remain
in the upstream dependency.

Local changes add source and package comments, rename the executable in help,
remove the production stub database import, rename the private `print` argument
to `printPaths`, and suppress Main's complexity lint to preserve upstream
dispatch. Review these three files when updating upstream.

### Upstream license

The MIT License (MIT)

Original Work
Copyright (c) 2016 Matthias Kadenbach
https://github.com/mattes/migrate

Modified Work
Copyright (c) 2018 Dale Hui
https://github.com/golang-migrate/migrate

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in
all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
THE SOFTWARE.

## OceanBase Go client dependency

`database/oceanbase` imports
[`github.com/cocosip/go-oceanbase v0.1.5`](https://github.com/cocosip/go-oceanbase/tree/v0.1.5).
Its module archive includes an MIT license, copyright 2026 go-oceanbase
contributors. Client source is used as a Go module dependency rather than copied
into this repository.

## DM Go client dependency

`database/dm` imports [gitee.com/chunanyong/dm v1.8.23](https://gitee.com/chunanyong/dm/tree/v1.8.23).
The inspected module archive corresponds to revision
`8ba262af5d7ddcd733eb21e7485f8831689d8c28`; its source files carry Dameng
Database Co., Ltd. copyright notices. The archive contains no LICENSE file.
This project does not attribute MIT terms to that dependency; consult its
maintainer/vendor for the applicable redistribution terms. Native client source
is used as a Go module dependency rather than copied into this repository.

## Huawei GaussDB Go client dependency

`database/gaussdb` imports
[`github.com/HuaweiCloudDeveloper/gaussdb-go v1.0.0-rc1`](https://github.com/huaweicloud-samples/database-gaussdb-go/tree/v1.0.0-rc1),
revision `1eccdbac5e7790f8c13668fd2506c8b712c12993`. The repository and declared
module names differ. Client source is used as a Go module dependency rather
than copied into this repository. Its module archive includes this license:

Copyright (c) 2013-2021 Jack Christensen

MIT License

Permission is hereby granted, free of charge, to any person obtaining
a copy of this software and associated documentation files (the
"Software"), to deal in the Software without restriction, including
without limitation the rights to use, copy, modify, merge, publish,
distribute, sublicense, and/or sell copies of the Software, and to
permit persons to whom the Software is furnished to do so, subject to
the following conditions:

The above copyright notice and this permission notice shall be
included in all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND,
EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF
MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND
NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE
LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION
OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION
WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.

## openGauss Go client dependency

`database/opengauss` imports
[`gitcode.com/opengauss/openGauss-connector-go-pq v1.0.9`](https://gitcode.com/opengauss/openGauss-connector-go-pq/tree/v1.0.9),
the official openGauss Go client, a fork of `github.com/lib/pq` with openGauss
SHA256/SM3 password authentication. The project historically published the
module under `gitee.com/opengauss/openGauss-connector-go-pq`; releases from
2025 declare the gitcode.com module path used here. Client source is used as a
Go module dependency rather than copied into this repository. Its module
archive includes this license (MIT, inherited from lib/pq):

Copyright (c) 2011-2013, 'pq' Contributors
Portions Copyright (C) 2011 Blake Mizerany

Permission is hereby granted, free of charge, to any person obtaining a copy of this software and associated documentation files (the "Software"), to deal in the Software without restriction, including without limitation the rights to use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of the Software, and to permit persons to whom the Software is furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be
included in all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND,
EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF
MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND
NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE
LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION
OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION
WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.
