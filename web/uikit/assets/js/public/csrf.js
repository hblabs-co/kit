// Double-submit CSRF helper — client half of web/csrf.
//
// The server leaves a random token in a cookie on every page view and
// never puts it in the (cacheable) HTML. At submit time this script
// copies the cookie into the form's hidden field, and the POST handler
// checks that both match. Mark each form and give it the hidden field:
//
//   <form method="post" action="/contact" data-csrf-form>
//     <input type="hidden" name="_csrf" value="">
//     ...
//   </form>
//   <script src="{{ asset "uikit/js/public/csrf.js" }}" defer></script>
//
// Defaults match csrf.DefaultCookie / csrf.DefaultField; a form can
// override them with data-csrf-cookie="..." and data-csrf-field="...".
(function () {
  "use strict";

  function cookie(name) {
    var parts = document.cookie ? document.cookie.split("; ") : [];
    for (var i = 0; i < parts.length; i++) {
      var kv = parts[i].split("=");
      if (kv[0] === name) return decodeURIComponent(kv.slice(1).join("="));
    }
    return "";
  }

  document.querySelectorAll("form[data-csrf-form]").forEach(function (form) {
    form.addEventListener("submit", function () {
      var name = form.getAttribute("data-csrf-field") || "_csrf";
      var field = form.querySelector('input[name="' + name + '"]');
      if (field) field.value = cookie(form.getAttribute("data-csrf-cookie") || "hbl_csrf");
    });
  });
})();
