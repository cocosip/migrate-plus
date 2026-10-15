# Third-Party Notices

migrate-plus is licensed under its own [MIT license](LICENSE), copyright
2026 cocosip. The following notice applies to the derived CLI files.

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
