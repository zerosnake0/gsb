$(function () {
    $("form").submit(function (ev) {
        $(this).find("button").prop("disabled", "disabled");
        return true;
    })
});
