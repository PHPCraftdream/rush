package callbeat

import "charm.land/fantasy"

func WrapLanguageModelForTest(inner fantasy.LanguageModel) fantasy.LanguageModel {
	return &hbLanguageModel{inner: inner}
}
