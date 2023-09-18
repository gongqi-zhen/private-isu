/*
概要
isucon solo play用portal

使い方
Spreadsheetのコンテナバインドスクリプトとして準備する
WebAPPとしてdeployしPOST_URLを準備する
bench.shの結果をSpreadsheetでグラフ化する

問題
doPostでreturnすると
現在、ファイルを開くことができません。というレスポンスと共にPOSTも失敗する
そのためエラー処理を省略しreturnしていない。solo play用なので許容する
*/

function doPost(e) {

  var jsonData = JSON.parse(e.postData.contents);
  recordJSONData(jsonData);

}

function isValidJSON(data) {
  // Add validation rules as per your requirements
  if (typeof data.pass === "boolean" &&
    typeof data.score === "number" &&
    typeof data.success === "number" &&
    typeof data.fail === "number" &&
    Array.isArray(data.message)) {
    return true;
  }
  return false;
}

function recordJSONData(data) {
  var SHEET_NAME = 'Sheet1';

  var ss = SpreadsheetApp.getActiveSpreadsheet();
  var sheet = ss.getSheetByName(SHEET_NAME);

  // ヘッダが存在しない場合は初期化
  if (sheet.getLastRow() === 0) {
    sheet.appendRow(['Pass', 'Date', 'Score', 'Success', 'Fail', 'Messages']);
  }

  // データを抽出
  var pass = data.pass;
  var date = Utilities.formatDate(new Date(), 'JST', 'yyyy-MM-dd HH:mm:ss');
  var score = data.score;
  var success = data.success;
  var fail = data.fail;
  var messages = data.messages.join(', ');

  // 新しい行にデータを追加
  sheet.appendRow([pass, date, score, success, fail, messages]);
}

function createErrorResponse(message) {
  return ContentService.createTextOutput(JSON.stringify({
    status: "error",
    message: message
  })).setMimeType(ContentService.MimeType.JSON);
}