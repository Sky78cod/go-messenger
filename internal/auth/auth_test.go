package auth

import "testing"

func TestPassword(t *testing.T) {
	hash, err := HashPassword("VerySecureExample123!")
	if err != nil {
		t.Fatal(err)
	}
	ok, err := CheckPassword("VerySecureExample123!", hash)
	if err != nil || !ok {
		t.Fatalf("правильный пароль отвергнут: ok=%v err=%v", ok, err)
	}
	ok, err = CheckPassword("IncorrectPassword123!", hash)
	if err != nil || ok {
		t.Fatalf("неверный пароль принят: ok=%v err=%v", ok, err)
	}
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("ожидалась ошибка для короткого пароля")
	}
}

func TestSessionToken(t *testing.T) {
	one, hashOne, err := GenerateSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	two, hashTwo, err := GenerateSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	if one == two || hashOne == hashTwo || one == hashOne {
		t.Fatal("токен неслучайный или совпадает с хешем")
	}
	if err := ValidateSessionToken(one); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSessionToken("hello"); err == nil {
		t.Fatal("принят неверный токен")
	}
}
