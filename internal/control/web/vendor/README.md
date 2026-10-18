# Browser Markdown dependencies

Vendored from the npm registry, with exact versions and tarball SHA-512 integrity in `versions.json`. Original license files are included. Both scripts are embedded in the Go Gateway and served from the same origin; the browser does not depend on a CDN.

- [Marked documentation](https://marked.js.org/): Markdown / GFM parsing.
- [DOMPurify](https://github.com/cure53/DOMPurify): HTML filtering before inserting a DOM fragment.

`renderText` allows only chat formatting elements. Code remains literal text, links open separately, task checkboxes are disabled, and tables scroll horizontally when needed. Raw scripts, images, event handlers, and unsafe link protocols are not rendered.
